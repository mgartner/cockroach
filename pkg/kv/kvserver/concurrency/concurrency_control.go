// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package concurrency provides a concurrency manager structure that
// encapsulates the details of concurrency control and contention handling for
// serializable key-value transactions.
package concurrency

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency/lock"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency/poison"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/lockspanset"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/spanset"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/txnwait"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/enginepb"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
)

// Manager is a structure that sequences incoming requests and provides
// isolation between requests that intend to perform conflicting operations.
// During sequencing, conflicts are discovered and any found are resolved
// through a combination of passive queuing and active pushing. Once a request
// has been sequenced, it is free to evaluate without concerns of conflicting
// with other in-flight requests due to the isolation provided by the manager.
// This isolation is guaranteed for the lifetime of the request but terminates
// once the request completes.
//
// Transactions require isolation both within requests and across requests. The
// manager accommodates this by allowing transactional requests to acquire
// locks, which outlive the requests themselves. Locks extend the duration of
// the isolation provided over specific keys to the lifetime of the lock-holder
// transaction itself. They are (typically) only released when the transaction
// commits or aborts. Other requests that find these locks while being sequenced
// wait on them to be released in a queue before proceeding. Because locks are
// checked during sequencing, requests are guaranteed access to all declared
// keys after they have been sequenced. In other words, locks don't need to be
// checked again during evaluation.
//
// However, at the time of writing, not all locks are stored directly under the
// manager's control, so not all locks are discoverable during sequencing.
// Specifically, write intents (replicated, exclusive locks) are stored inline
// in the MVCC keyspace, so they are not detectable until request evaluation
// time. To accommodate this form of lock storage, the manager exposes a
// HandleLockConflictError method, which can be used in conjunction with a retry
// loop around evaluation to integrate external locks with the concurrency
// manager structure. In the future, we intend to pull all locks, including
// those associated with write intents, into the concurrency manager directly
// through a replicated lock table structure.
//
// Fairness is ensured between requests. In general, if any two requests
// conflict then the request that arrived first will be sequenced first. As
// such, sequencing guarantees FIFO semantics. The primary exception to this is
// that a request that is part of a transaction which has already acquired a
// lock does not need to wait on that lock during sequencing, and can therefore
// ignore any queue that has formed on the lock. For other exceptions, see the
// later comment for lockTable.
//
// # Internal Components
//
// The concurrency manager is composed of a number of internal synchronization,
// bookkeeping, and queueing structures. Each of these is discussed in more
// detail on their interface definition. The following diagram details how the
// components are tied together:
//
//	+---------------------+---------------------------------------------+
//	| concurrency.Manager |                                             |
//	+---------------------+                                             |
//	|                                                                   |
//	+------------+  acquire  +--------------+        acquire            |
//	  Sequence() |--->--->---| latchManager |<---<---<---<---<---<---+  |
//	+------------+           +--------------+                        |  |
//	|                         / check locks + wait queues            |  |
//	|                        v  if conflict, enter q & drop latches  ^  |
//	|         +---------------------------------------------------+  |  |
//	|         | [ lockTable ]                                     |  |  |
//	|         | [    key1   ]    -------------+-----------------+ |  ^  |
//	|         | [    key2   ]  /  keyLocks:   | lockWaitQueue:  |----<---<---<----+
//	|         | [    key3   ]-{   - lock type | +-[a]<-[b]<-[c] | |  |  |         |
//	|         | [    key4   ]  \  - txn  meta | |  (no latches) |-->-^  |         |
//	|         | [    key5   ]    -------------+-|---------------+ |     |         |
//	|         | [    ...    ]                   v                 |     |         ^
//	|         +---------------------------------|-----------------+     |         | if lock found, HandleLockConflictError()
//	|                 |                         |                       |         |  - enter lockWaitQueue
//	|                 |       +- may be remote -+--+                    |         |  - drop latches
//	|                 |       |                    |                    |         |  - wait for lock update / release
//	|                 v       v                    ^                    |         |
//	|                 |    +--------------------------+                 |         ^
//	|                 |    | txnWaitQueue:            |                 |         |
//	|                 |    | (located on txn record's |                 |         |
//	|                 v    |  leaseholder replica)    |                 |         |
//	|                 |    |--------------------------|                 |         ^
//	|                 |    | [txn1] [txn2] [txn3] ... |----<---<---<---<----+     |
//	|                 |    +--------------------------+                 |   | if txn push failed, HandleTransactionPushError()
//	|                 |                                                 |   |  - enter txnWaitQueue
//	|                 |                                                 |   ^  - drop latches
//	|                 |                                                 |   |  - wait for txn record update
//	|                 |                                                 |   |     |
//	|                 |                                                 |   |     |
//	|                 +--> retain latches --> remain at head of queues ---> evaluate ---> Finish()
//	|                                                                   |
//	+----------+                                                        |
//	  Finish() | ---> exit wait queues ---> drop latches -----------------> respond ...
//	+----------+                                                        |
//	|                                                                   |
//	+-------------------------------------------------------------------+
//
// See the comments on individual components for a more detailed look at their
// interface and inner-workings.
//
// At a high-level, a request enters the concurrency manager and immediately
// acquires latches from the latchManager to serialize access to the keys that
// it intends to touch. This latching takes into account the keys being
// accessed, the MVCC timestamp of accesses, and the access method being used
// (read vs. write) to allow for concurrency where possible. This has the effect
// of queuing on conflicting in-flight operations until their completion.
//
// Once latched, the request consults the lockTable to check for any conflicting
// locks owned by other transactions. If any are found, the request enters the
// corresponding lockWaitQueue and its latches are dropped. Requests in the
// queue wait for the corresponding lock to be released by intent resolution.
// While waiting, the head of the lockWaitQueue pushes the owner of the lock
// through a remote RPC that ends up in the pushee's txnWaitQueue. This queue
// exists on the leaseholder replica of the range that contains the pushee's
// transaction record. Other entries in the queue wait for the head of the
// queue, eventually pushing it to detect coordinator failures and transaction
// deadlocks. Once the lock is released, the head of the queue reacquires
// latches and attempts to proceed while remaining at the head of that
// lockWaitQueue to ensure fairness.
//
// Once a request is latched and observes no conflicting locks in the lockTable
// and no conflicting lockWaitQueues that it is not already the head of, the
// request can proceed to evaluate. During evaluation, the request may insert or
// remove locks from the lockTable for its own transaction.
//
// When the request completes, it exits any lockWaitQueues that it was a part of
// and releases its latches. However, if the request was successful, any locks
// that it inserted into the lockTable remain.
type Manager interface {
	RequestSequencer
	ContentionHandler
	LockManager
	TransactionManager
	RangeStateListener
	MetricExporter
	TestingAccessor
}

// RequestSequencer is concerned with the sequencing of concurrent requests. It
// is one of the roles of Manager.
type RequestSequencer interface {
	// SequenceReq acquires latches, checks for locks, and queues behind and/or
	// pushes other transactions to resolve any conflicts. Once sequenced, the
	// request is guaranteed sufficient isolation for the duration of its
	// evaluation, until the returned request guard is released. NOTE: this last
	// part will not be true until replicated locks are pulled into the
	// concurrency manager. This is the normal behavior for a request marked as
	// PessimisticEval. For OptimisticEval, it can optimize by not acquiring
	// locks, and the request must call Guard.CheckOptimisticNoConflicts after
	// evaluation. OptimisticEval is only permitted in the first call to
	// SequenceReq for a Request. A failed OptimisticEval must use
	// PessimisticAfterFailedOptimisticEval, for the immediately following
	// SequenceReq call, if the latches are already held.
	// TODO(sumeer): change OptimisticEval to only queue the latches and not
	// wait for them, so PessimisticAfterFailedOptimisticEval will wait for
	// them.
	//
	// An optional existing request guard can be provided to SequenceReq. This
	// allows the request's position in lock wait-queues to be retained across
	// sequencing attempts. If provided, the guard should not be holding latches
	// already (for PessimisticEval). The expected usage of this parameter is
	// that it will only be provided after acquiring a Guard from a
	// ContentionHandler method.
	//
	// If the method returns a non-nil request guard then the caller must ensure
	// that the guard is eventually released by passing it to FinishReq.
	//
	// Alternatively, the concurrency manager may be able to serve the request
	// directly, in which case it will return a Response for the request. If it
	// does so, it will not return a request guard.
	SequenceReq(context.Context, *Guard, Request, RequestEvalKind) (*Guard, Response, *Error)

	// PoisonReq idempotently marks a Guard as poisoned, indicating that its
	// latches may be held for an indefinite amount of time. Requests waiting on
	// this Guard will be notified. Latch acquisitions under poison.Policy_Error
	// react to this by failing with a poison.PoisonedError, while requests under
	// poison.Policy_Wait continue waiting, but propagate the poisoning upwards.
	//
	// See poison.Policy for details.
	PoisonReq(*Guard)

	// FinishReq marks the request as complete, releasing any protection
	// the request had against conflicting requests and allowing conflicting
	// requests that are blocked on this one to proceed. The guard should not
	// be used after being released.
	FinishReq(context.Context, *Guard)
}

// ContentionHandler is concerned with handling contention-related errors. This
// typically involves preparing the request to be queued upon a retry. It is one
// of the roles of Manager.
type ContentionHandler interface {
	// HandleLockConflictError consumes a LockConflictError by informing the
	// concurrency manager about the replicated write intent that was missing
	// from its lock table which was found during request evaluation (while
	// holding latches) under the provided lease sequence. After doing so, it
	// enqueues the request that hit the error in the lock's wait-queue (but
	// does not wait) and releases the guard's latches. It returns an updated
	// guard reflecting this change. After the method returns, the original
	// guard should no longer be used. If an error is returned then the provided
	// guard will be released and no guard will be returned.
	//
	// Example usage: Txn A scans the lock table and does not see an intent on
	// key K from txn B because the intent is not being tracked in the lock
	// table. Txn A moves on to evaluation. While scanning, it notices the
	// intent on key K. It throws a LockConflictError which is consumed by this
	// method before txn A retries its scan. During the retry, txn A scans the
	// lock table and observes the lock on key K, so it enters the lock's
	// wait-queue and waits for it to be resolved.
	HandleLockConflictError(
		context.Context, *Guard, roachpb.LeaseSequence, *kvpb.LockConflictError,
	) (*Guard, *Error)

	// HandleTransactionPushError consumes a TransactionPushError thrown by a
	// PushTxnRequest by informing the concurrency manager about a transaction
	// record that could not be pushed during request evaluation (while holding
	// latches). After doing so, it releases the guard's latches. It returns an
	// updated guard reflecting this change. After the method returns, the
	// original guard should no longer be used.
	//
	// Example usage: Txn A sends a PushTxn request to push abort txn B. When
	// the request is originally sequenced through the concurrency manager, it
	// checks the txn wait-queue and finds that txn B is not being tracked, so
	// it does not queue up behind it. Txn A moves on to evaluation and tries to
	// push txn B's record. This push fails because txn B is not expired, which
	// results in a TransactionPushError. This error is consumed by this method
	// before txn A retries its push. During the retry, txn A finds that txn B
	// is being tracked in the txn wait-queue so it waits there for txn B to
	// finish.
	HandleTransactionPushError(context.Context, *Guard, *kvpb.TransactionPushError) *Guard
}

// LockManager is concerned with tracking locks that are stored on the manager's
// range. It is one of the roles of Manager.
type LockManager interface {
	// OnLockAcquired informs the concurrency manager that a transaction has
	// acquired a new lock or re-acquired an existing lock that it already held.
	OnLockAcquired(context.Context, *roachpb.LockAcquisition)

	// OnLockMissing informs the concurrency manager that a lock has been reported
	// missing to a client via QueryIntent. Such locks cannot later be
	// materialized via a lock table flush.
	OnLockMissing(context.Context, *roachpb.LockAcquisition)

	// OnLockUpdated informs the concurrency manager that a transaction has
	// updated or released a lock or range of locks that it previously held.
	// The Durability field of the lock update struct is ignored.
	OnLockUpdated(context.Context, *roachpb.LockUpdate)

	// QueryLockTableState gathers detailed metadata on locks tracked in the lock
	// table that are part of the provided span and key scope, up to provided limits.
	QueryLockTableState(ctx context.Context, span roachpb.Span, opts QueryLockTableOptions) ([]roachpb.LockStateInfo, QueryLockTableResumeState)

	// ExportUnreplicatedLocks runs exporter on each held, unreplicated lock
	// in the given span.
	ExportUnreplicatedLocks(span roachpb.Span, exporter func(*roachpb.LockAcquisition))
}

// TransactionManager is concerned with tracking transactions that have their
// record stored on the manager's range. It is one of the roles of Manager.
type TransactionManager interface {
	// OnTransactionUpdated informs the concurrency manager that a transaction's
	// status was updated.
	OnTransactionUpdated(context.Context, *roachpb.Transaction)

	// GetDependents returns a set of transactions waiting on the specified
	// transaction either directly or indirectly. The method is used to perform
	// deadlock detection. See txnWaitQueue for more.
	GetDependents(uuid.UUID) []uuid.UUID
}

// RangeStateListener is concerned with observing updates to the concurrency
// manager's range. It is one of the roles of Manager.
type RangeStateListener interface {
	// OnRangeDescUpdated informs the manager that its range's descriptor has been
	// updated.
	OnRangeDescUpdated(*roachpb.RangeDescriptor)

	// OnRangeLeaseTransferEval informs the concurrency manager that the range is
	// evaluating a lease transfer. It is called during evalutation of a lease
	// transfer. The returned LockAcquisition structs represent held locks that we
	// may want to flush to disk as replicated. Since lease transfers declare
	// latches that conflict with all requests, the caller knows that nothing is
	// going to modify the lock table as its evaluating.
	OnRangeLeaseTransferEval() ([]*roachpb.LockAcquisition, int64)

	// OnRangeSubsumeEval informs the concurrency manager that the range is
	// evaluating a merge.
	OnRangeSubsumeEval() ([]*roachpb.LockAcquisition, int64)

	// OnRangeLeaseUpdated informs the concurrency manager that its range's
	// lease has been updated. The argument indicates whether this manager's
	// replica is the leaseholder going forward.
	OnRangeLeaseUpdated(_ roachpb.LeaseSequence, isLeaseholder bool)

	// OnRangeSplit informs the concurrency manager that its range
	// has split off a new range to its RHS. The provided key
	// should be the new RHS StartKey (LHS EndKey). Note that this
	// is inclusives so all locks on keys greater or equal to this
	// key will be cleared. The returned LockAcquistion structs
	// represent locks that we may want to acquire on the RHS
	// replica before it is serving requests.
	OnRangeSplit(roachpb.Key) []roachpb.LockAcquisition

	// OnRangeMerge informs the concurrency manager that its range has merged
	// into its LHS neighbor. This is not called on the LHS range being merged
	// into.
	OnRangeMerge()

	// OnReplicaSnapshotApplied informs the concurrency manager that its replica
	// has received a snapshot from another replica in its range.
	OnReplicaSnapshotApplied()
}

// MetricExporter is concerned with providing observability into the state of
// the concurrency manager. It is one of the roles of Manager.
type MetricExporter interface {
	// LatchMetrics returns information about the state of the latchManager.
	LatchMetrics() LatchMetrics

	// LockTableMetrics returns information about the state of the lockTable.
	LockTableMetrics() LockTableMetrics

	// TODO(nvanbenschoten): provide better observability into the state of the
	// txn wait queue. Currently, all observability is provided by metrics that
	// are passed to the txn wait queue constructor.
	// TxnWaitQueueMetrics()
}

// TestingAccessor is concerned with providing testing hooks that expose the
// state of the concurrency manager, to be used by unit tests outside of the
// concurrency package. It is one of the roles of Manager.
type TestingAccessor interface {
	// TestingLockTableString returns a debug string representing the state of the
	// lockTable.
	TestingLockTableString() string

	// TestingTxnWaitQueue returns the concurrency manager's txnWaitQueue.
	TestingTxnWaitQueue() *txnwait.Queue

	// TestingSetMaxLocks updates the locktable's lock limit. This can be used to
	// force the locktable to exceed its limit and clear locks.
	TestingSetMaxLocks(n int64)
}

///////////////////////////////////
// External API Type Definitions //
///////////////////////////////////

// RequestEvalKind informs the manager of the evaluation kind for the current
// evaluation attempt. Optimistic evaluation is used for requests involving
// limited scans, where the checking of locks and latches may be (partially)
// postponed until after evaluation, once the limit has been applied and the
// key spans have been constrained, using Guard.CheckOptimisticNoConflicts.
// Note that intents (replicated single-key locks) will still be observed
// during evaluation.
//
// The setting can change across different calls to SequenceReq. The
// permissible sequences are:
//   - OptimisticEval: when optimistic evaluation succeeds.
//   - OptimisticEval, PessimisticAfterFailedOptimisticEval, PessimisticEval*:
//     when optimistic evaluation failed.
//   - PessimisticEval+: when only pessimistic evaluation was attempted.
type RequestEvalKind int

const (
	// PessimisticEval represents pessimistic locking.
	PessimisticEval RequestEvalKind = iota
	// OptimisticEval represents optimistic locking.
	OptimisticEval
	// PessimisticAfterFailedOptimisticEval represents a request sequencing
	// attempt immediately following a failed OptimisticEval.
	PessimisticAfterFailedOptimisticEval
)

// Request is the input to Manager.SequenceReq. The struct contains all of the
// information necessary to sequence a KV request and determine which locks and
// other in-flight requests it conflicts with.
type Request struct {
	// The (optional) transaction that sent the request.
	// Non-transactional requests do not acquire locks.
	Txn *roachpb.Transaction

	// The timestamp that the request should evaluate at.
	// Should be set to Txn.ReadTimestamp if Txn is non-nil.
	Timestamp hlc.Timestamp

	// The priority of the request. Only set if Txn is nil.
	NonTxnPriority roachpb.UserPriority

	// The consistency level of the request. Only set if Txn is nil.
	ReadConsistency kvpb.ReadConsistencyType

	// The wait policy of the request. Signifies how the request should
	// behave if it encounters conflicting locks held by other active
	// transactions.
	WaitPolicy lock.WaitPolicy

	// The maximum amount of time that the batch request will wait while
	// attempting to acquire a lock on a key or while blocking on an
	// existing lock in order to perform a non-locking read on a key.
	LockTimeout time.Duration

	// The maximum length of a lock wait-queue that the request is willing
	// to enter and wait in. Used to provide a release valve and ensure some
	// level of quality-of-service under severe per-key contention. If set
	// to a non-zero value and an existing lock wait-queue is already equal
	// to or exceeding this length, the request will be rejected eagerly
	// with a WriteIntentError instead of entering the queue and waiting.
	MaxLockWaitQueueLength int

	// AdmissionHeader is the header in the request's BatchRequest. It is plumbed
	// through for intent resolution admission control.
	AdmissionHeader kvpb.AdmissionHeader

	// The poison.Policy to use for this Request.
	PoisonPolicy poison.Policy

	// The individual requests in the batch.
	Requests []kvpb.RequestUnion

	// The maximal set of spans that the request will access. Latches
	// will be acquired for these spans.
	//
	// Note: ownership of the SpanSet is assumed by the Request once it is
	// passed to SequenceReq. Only supplied to SequenceReq if the method is
	// not also passed an exiting Guard.
	LatchSpans *spanset.SpanSet

	// The maximal set of spans within which the request expects to have isolation
	// from conflicting transactions. The level of isolation for a span is
	// dictated by its corresponding lock Strength. Conflicting locks within these
	// spans will be queued on and conditionally pushed.
	//
	// Note: ownership of the LockSpanSet is assumed by the Request once it is
	// passed to SequenceReq. Only supplied to SequenceReq if the method is
	// not also passed an exiting Guard.
	LockSpans *lockspanset.LockSpanSet

	// Batch is the batch to which the request belongs.
	Batch *kvpb.BatchRequest

	// DeadlockTimeout is the amount of time that the request will wait on a lock
	// before pushing the lock holder's transaction for deadlock detection.
	DeadlockTimeout time.Duration
}

// Guard is returned from Manager.SequenceReq. The guard is passed back in to
// Manager.FinishReq to release the request's resources when it has completed.
type Guard struct {
	Req Request
	lg  latchGuard
	lm  latchManager
	ltg lockTableGuard
	// The latest RequestEvalKind passed to SequenceReq.
	EvalKind RequestEvalKind
}

// Response is a slice of responses to requests in a batch. This type is used
// when the concurrency manager is able to respond to a request directly during
// sequencing.
type Response = []kvpb.ResponseUnion

// Error is an alias for a kvpb.Error.
type Error = kvpb.Error

// QueryLockTableOptions bundles the options for the QueryLockTableState function.
type QueryLockTableOptions struct {
	MaxLocks           int64
	TargetBytes        int64
	IncludeUncontended bool
}

// QueryLockTableResumeState bundles the return metadata on the pagination of
// results from the QueryLockTableState function.
type QueryLockTableResumeState struct {
	ResumeSpan   *roachpb.Span
	ResumeReason kvpb.ResumeReason

	// ResumeNextBytes represents the size (in bytes) of the next
	// roachpb.LockStateInfo object that would have been returned by
	// QueryLockTableState if it were not limited by MaxLocks or TargetBytes.
	// As such, this value is only set if ResumeSpan, ResumeReason have been set.
	ResumeNextBytes int64

	// TotalBytes is the byte size of the roachpb.LockStateInfo objects returned
	// by QueryLockTableState, and is always set. This value is used to calculate
	// the remaining quota of bytes (from TargetBytes) that can be used in
	// querying other ranges served by the same request.
	TotalBytes int64
}

///////////////////////////////////
// Internal Structure Interfaces //
///////////////////////////////////

// latchManager serializes access to keys and key ranges. The
// {AcquireOptimistic,CheckOptimisticNoConflicts,WaitUntilAcquired} methods
// are only for use in optimistic latching.
//
// See additional documentation in pkg/storage/spanlatch.
type latchManager interface {
	// Acquires latches, providing mutual exclusion for conflicting requests.
	Acquire(context.Context, Request) (latchGuard, *Error)

	// AcquireOptimistic is like Acquire in that it inserts latches, but it does
	// not wait for conflicting latches on overlapping spans to be released
	// before returning. This should be followed by CheckOptimisticNoConflicts
	// to validate that not waiting did not violate correctness.
	AcquireOptimistic(req Request) latchGuard

	// CheckOptimisticNoConflicts returns true iff the spans in the provided
	// spanset do not conflict with existing latches.
	CheckOptimisticNoConflicts(lg latchGuard, spans *spanset.SpanSet) bool

	// WaitUntilAcquired is meant to be called when CheckOptimisticNoConflicts
	// returned false, or some other occurrence (like conflicting locks) is
	// causing this request to switch to pessimistic latching.
	WaitUntilAcquired(ctx context.Context, lg latchGuard) (latchGuard, *Error)

	// WaitFor waits for conflicting latches on the specified spans without adding
	// any latches itself. Fast path for operations that only require flushing out
	// old operations without blocking any new ones.
	WaitFor(
		ctx context.Context,
		spans *spanset.SpanSet,
		pp poison.Policy,
		ba *kvpb.BatchRequest,
	) *Error

	// Poison a guard's latches, allowing waiters to fail fast.
	Poison(latchGuard)

	// Release a guard's latches, relinquish its protection from conflicting requests.
	Release(ctx context.Context, lg latchGuard)

	// Metrics returns information about the state of the latchManager.
	Metrics() LatchMetrics
}

// latchGuard is a handle to a set of acquired key latches.
type latchGuard interface{}

// lockTable holds a collection of locks acquired by in-progress transactions.
// Each lock in the table has a possibly-empty lock wait-queue associated with
// it, where conflicting transactions can queue while waiting for the lock to be
// released.
//
//	+---------------------------------------------------+
//	| [ lockTable ]                                     |
//	| [    key1   ]    -------------+-----------------+ |
//	| [    key2   ]  /  keyLocks:   | lockWaitQueue:  | |
//	| [    key3   ]-{   - lock type | <-[a]<-[b]<-[c] | |
//	| [    key4   ]  \  - txn meta  |                 | |
//	| [    key5   ]    -------------+-----------------+ |
//	| [    ...    ]                                     |
//	+---------------------------------------------------+
//
// The database is read and written using "requests". Transactions are composed
// of one or more requests. Isolation is needed across requests. Additionally,
// since transactions represent a group of requests, isolation is needed across
// such groups. Part of this isolation is accomplished by maintaining multiple
// versions and part by allowing requests to acquire locks. Even the isolation
// based on multiple versions requires some form of mutual exclusion to ensure
// that a read and a conflicting lock acquisition do not happen concurrently.
// The lock table provides both locking and sequencing of requests (in concert
// with the use of latches). The lock table sequences both transactional and
// non-transactional requests, but the latter cannot acquire locks.
//
// Locks outlive the requests themselves and thereby extend the duration of the
// isolation provided over specific keys to the lifetime of the lock-holder
// transaction itself. They are (typically) only released when the transaction
// commits or aborts. Other requests that find these locks while being sequenced
// wait on them to be released in a queue before proceeding. Because locks are
// checked during sequencing, requests are guaranteed access to all declared
// keys after they have been sequenced. In other words, locks don't need to be
// checked again during evaluation.
//
// However, at the time of writing, not all locks are stored directly under
// lock table control, so not all locks are discoverable during sequencing.
// Specifically, write intents (replicated, exclusive locks) are stored inline
// in the MVCC keyspace, so they are often not detectable until request
// evaluation time. To accommodate this form of lock storage, the lock table
// exposes an AddDiscoveredLock method. In the future, we intend to pull all
// locks, including those associated with write intents, into the lock table
// directly.
//
// The lock table also provides fairness between requests. If two requests
// conflict then the request that arrived first will typically be sequenced
// first. There are some exceptions:
//
//   - a request that is part of a transaction which has already acquired a lock
//     does not need to wait on that lock during sequencing, and can therefore
//     ignore any queue that has formed on the lock.
//
//   - contending requests that encounter different levels of contention may be
//     sequenced in non-FIFO order. This is to allow for more concurrency. e.g.
//     if request R1 and R2 contend on key K2, but R1 is also waiting at key K1,
//     R2 could slip past R1 and evaluate.
type lockTable interface {
	requestQueuer

	// ScanAndEnqueue scans over the spans that the request will access and
	// enqueues the request in the lock wait-queue of any conflicting locks
	// encountered.
	//
	// The first call to ScanAndEnqueue for a given request uses a nil
	// lockTableGuard and the subsequent calls reuse the previously returned
	// one. The latches needed by the request must be held when calling this
	// function.
	ScanAndEnqueue(Request, lockTableGuard) (lockTableGuard, *Error)

	// ScanOptimistic takes a snapshot of the lock table for later checking for
	// conflicts, and returns a guard. It is for optimistic evaluation of
	// requests that will typically scan a small subset of the spans mentioned
	// in the Request. After Request evaluation, CheckOptimisticNoConflicts
	// must be called on the guard.
	ScanOptimistic(Request) lockTableGuard

	// Dequeue removes the request from its lock wait-queues. It should be
	// called when the request is finished, whether it evaluated or not. The
	// guard should not be used after being dequeued.
	//
	// This method does not release any locks. This method must be called on the
	// last guard returned from ScanAndEnqueue for the request, even if one of
	// the (a) lockTable calls that use a lockTableGuard parameter, or (b) a
	// lockTableGuard call, returned an error. The method allows but does not
	// require latches to be held.
	Dequeue(lockTableGuard)

	// AddDiscoveredLock informs the lockTable of a lock which is wasn't
	// previously tracking that was discovered during evaluation under the
	// provided lease sequence.
	//
	// The method is called when an exclusive replicated lock held by a
	// different transaction is discovered when reading the MVCC keys during
	// evaluation of this request. It adds the lock and enqueues this requester
	// in its wait-queue. It is required that request evaluation discover such
	// locks before acquiring its own locks, since the request needs to repeat
	// ScanAndEnqueue. When consultTxnStatusCache=true, and the transaction
	// holding the lock is known to be pushed or finalized, the lock is not added
	// to the lock table and instead tracked in the list of locks to resolve in
	// the lockTableGuard.
	//
	// The lease sequence is used to detect lease changes between the when
	// request that found the lock started evaluating and when the discovered
	// lock is added to the lockTable. If there has been a lease change between
	// these times, information about the discovered lock may be stale, so it is
	// ignored.
	//
	// A latch consistent with the access desired by the guard must be held on
	// the span containing the discovered lock's key.
	//
	// The method returns a boolean indicating whether the discovered lock was
	// properly handled either by adding it to the lockTable or storing it in
	// the list of locks to resolve in the lockTableGuard (both cases return
	// true) or whether it was ignored because the lockTable is currently
	// disabled (false).
	AddDiscoveredLock(
		foundLock *roachpb.Lock, seq roachpb.LeaseSequence,
		consultTxnStatusCache bool, guard lockTableGuard,
	) (bool, error)

	// AcquireLock informs the lockTable that a new lock was acquired or an
	// existing lock was updated.
	//
	// The TxnMeta associated with the lock acquisition must be the same one used
	// when the request scanned the lockTable initially. It must only be called in
	// the evaluation phase, before calling Dequeue, which means all latches
	// needed by the request are held. The key must be in the request's lock span
	// set with the appropriate strength. Currently, the only strength with which a
	// lock can be acquired is Intent[1]. This contract ensures that the lock is
	// not held in a conflicting manner by a different transaction.
	// Acquiring a lock that is already held by a transaction upgrades the lock's
	// timestamp and strength. Any prior sequence numbers at which the lock was
	// previously held and has since been rolled back are no longer tracked as
	// well.
	//
	// [1] We are in an intermediate state where KV ignores lock strength supplied
	// by SQL and acquires all locks with Intent locking strength. Notably, this
	// includes in-memory Exclusive locks acquired for `SELECT FOR UPDATE` queries
	// as well. While the Intent strength verbiage might be a bit surprising,
	// there is no meaningful difference when it comes to conflict resolution
	// between a lock and a request. This is a temporary state, which is bound to
	// change as we go about supporting additional locking strengths in the lock
	// table.
	//
	// For replicated locks, this must be called after the corresponding write
	// intent has been applied to the replicated state machine.
	AcquireLock(*roachpb.LockAcquisition) error

	// MarkIneligibleForExport marks any locks held by this transaction on the
	// same key as ineligible for export from the lock table for replication since
	// doing so could result in a transaction being erroneously committed.
	MarkIneligibleForExport(*roachpb.LockAcquisition) error

	// UpdateLocks informs the lockTable that an existing lock or range of locks
	// was either updated or released.
	//
	// The method is called during intent resolution. For spans containing
	// Replicated locks, this must be called after intent resolution has been
	// applied to the replicated state machine. The method itself, however,
	// ignores the Durability field in the LockUpdate. It can therefore be
	// used to update locks for a given transaction for all durability levels.
	//
	// A latch with SpanReadWrite must be held on span with the lowest timestamp
	// at which any of the locks could be held. This is explained below.
	//
	// Note that spans can be wider than the actual keys on which locks were
	// acquired, and it is ok if no locks are found or locks held by other
	// transactions are found (for those lock this call is a noop).
	//
	// For COMMITTED or ABORTED transactions, all locks are released.
	//
	// For PENDING or STAGING transactions, the behavior is:
	//
	// - All replicated locks known to the lockTable are dropped. This is not
	//   because those intents are necessarily deleted, but because in the
	//   current code where intents are not managed by the lockTable (this will
	//   change when we have a segregated lock table), we do not want to risk
	//   code divergence between lockTable and mvccResolveWriteIntent: the
	//   danger is that the latter removes or changes an intent while the
	//   lockTable retains it, and a waiter is stuck forever.
	//
	//   Note that even the conservative behavior of dropping locks requires
	//   that intent resolution acquire latches using the oldest timestamp at
	//   which the intent could have been written: if the intent was at ts=5 and
	//   the intent resolution is using ts=10 (since the transaction has been
	//   pushed), there is a race where a reader at ts=8 can be concurrently
	//   holding latches and the following bad sequence occurs (both thread1 and
	//   thread2 are concurrent since their latches do not conflict):
	//
	//   - [thread1-txn1] reader sees intent at ts=5
	//   - [thread2-txn2] intent resolution changes that intent to ts=10
	//   - [thread2-txn2] updateLocks is called and lock is removed since it is a
	//     replicated lock.
	//   - [thread1-txn1] reader calls addDiscoveredLock() for ts=5.
	//
	//   Now the lockTable thinks there is a lock and subsequent pushes of txn2
	//   by txn1 will do nothing since the txn2 is already at timestamp 10. Txn1
	//   will unnecessarily block until txn2 is done.
	//
	// - Unreplicated locks:
	//   - for epochs older than txn.Epoch, locks are dropped.
	//   - locks in the current epoch that are at a TxnMeta.Sequence
	//     contained in IgnoredSeqNums are dropped.
	//   - the remaining locks are changed to timestamp equal to
	//     txn.WriteTimestamp.
	UpdateLocks(*roachpb.LockUpdate) error

	// PushedTransactionUpdated informs the lock table that a transaction has been
	// pushed and is either finalized or has been moved to a higher timestamp.
	// This is used by the lock table in a best-effort manner to avoid waiting on
	// locks of finalized or pushed transactions and telling the caller via
	// lockTableGuard.ResolveBeforeScanning to resolve a batch of intents.
	PushedTransactionUpdated(*roachpb.Transaction)

	// QueryLockTableState returns detailed metadata on locks managed by the lockTable.
	QueryLockTableState(span roachpb.Span, opts QueryLockTableOptions) ([]roachpb.LockStateInfo, QueryLockTableResumeState)

	// ExportUnreplicatedLocks runs exporter on each held, unreplicated lock
	// in the given span.
	//
	// Note that the caller is responsible for acquiring latches across the span
	// it is exporting if it needs to be sure that the exported locks won't be
	// updated in the lock table while it is still referencing them.
	ExportUnreplicatedLocks(span roachpb.Span, exporter func(*roachpb.LockAcquisition))

	// Metrics returns information about the state of the lockTable.
	Metrics() LockTableMetrics

	// String returns a debug string representing the state of the lockTable.
	String() string

	// TestingSetMaxLocks updates the locktable's lock limit. This can be used to
	// force the locktable to exceed its limit and clear locks.
	TestingSetMaxLocks(maxLocks int64)
}

// lockTableGuard is a handle to a request as it waits on conflicting locks in a
// lockTable or as it holds a place in lock wait-queues as it evaluates.
type lockTableGuard interface {
	// ShouldWait must be called after each ScanAndEnqueue. The request should
	// proceed to evaluation if it returns false, else it releases latches and
	// listens to the channel returned by NewStateChan.
	ShouldWait() bool

	// NewStateChan returns the channel to listen on for notification that the
	// state may have changed. If ShouldWait returns true, this channel will
	// have an initial notification. Note that notifications are collapsed if
	// not retrieved, since it is not necessary for the waiter to see every
	// state transition.
	NewStateChan() chan struct{}

	// CurState returns the latest waiting state.
	CurState() (waitingState, error)

	// ResolveBeforeScanning lists the locks to resolve before scanning again.
	// This must be called after:
	// - the waiting state has transitioned to doneWaiting.
	// - if locks were discovered during evaluation, it must be called after all
	//   the discovered locks have been added.
	ResolveBeforeScanning() []roachpb.LockUpdate

	// CheckOptimisticNoConflicts uses the LockSpanSet representing the spans that
	// were actually read, to check for conflicting locks, after an optimistic
	// evaluation. It returns true if there were no conflicts. See
	// lockTable.ScanOptimistic for context. Note that the evaluation has
	// already seen any intents (replicated single-key locks) that conflicted,
	// so this checking is practically only going to find unreplicated locks
	// that conflict.
	CheckOptimisticNoConflicts(*lockspanset.LockSpanSet) (ok bool)

	// IsKeyLockedByConflictingTxn returns whether the specified key is locked by
	// a conflicting transaction in the lockTableGuard's snapshot of the lock
	// table, given the caller's own desired locking strength. If so, true is
	// returned and so is the lock holder. If the lock is held by the transaction
	// itself, there's no conflict to speak of, so false is returned.
	//
	// This method is used by requests in conjunction with the SkipLocked wait
	// policy to determine which keys they should skip over during evaluation.
	//
	// If the supplied lock strength is locking (!= lock.None), then any queued
	// locking requests that came before the lockTableGuard will also be checked
	// for conflicts. This helps prevent a stream of locking SKIP LOCKED requests
	// from starving out regular locking requests. In such cases, true is
	// returned, but so is nil.
	IsKeyLockedByConflictingTxn(context.Context, roachpb.Key, lock.Strength) (bool, *enginepb.TxnMeta, error)
}

// lockTableWaiter is concerned with waiting in lock wait-queues for locks held
// by conflicting transactions. It ensures that waiting requests continue to
// make forward progress even in the presence of faulty transaction coordinators
// and transaction deadlocks.
//
// The waiter implements logic for a request to wait on conflicting locks in the
// lockTable until they are released. Similarly, it implements logic to wait on
// conflicting requests ahead of the caller's request in any lock wait-queues
// that it is a part of.
//
// This waiting state responds to a set of state transitions in the lock table:
//   - a conflicting lock is released
//   - a conflicting lock is updated such that it no longer conflicts
//   - a conflicting request in the lock wait-queue acquires the lock
//   - a conflicting request in the lock wait-queue exits the lock wait-queue
//
// These state transitions are typically reactive - the waiter can simply wait
// for locks to be released or lock wait-queues to be exited by other actors.
// Reacting to state transitions for conflicting locks is powered by the
// LockManager and reacting to state transitions for conflicting lock
// wait-queues is powered by the RequestSequencer interface.
//
// However, in the case of transaction coordinator failures or transaction
// deadlocks, a state transition may never occur without intervention from the
// waiter. To ensure forward-progress, the waiter may need to actively push
// either a lock holder of a conflicting lock or the head of a conflicting lock
// wait-queue. This active pushing requires an RPC to the leaseholder of the
// conflicting transaction's record, and will typically result in the RPC
// queuing in that leaseholder's txnWaitQueue. Because this can be expensive,
// the push is not immediately performed. Instead, it is only performed after a
// delay.
type lockTableWaiter interface {
	// WaitOn accepts and waits on a lockTableGuard that has returned true from
	// ShouldWait.
	//
	// The method should be called after dropping any latches that a request
	// has acquired. It returns when the request is at the front of all lock
	// wait-queues and it is safe to re-acquire latches and scan the lockTable
	// again.
	WaitOn(context.Context, Request, lockTableGuard) *Error

	// ResolveDeferredIntents resolves the batch of intents if the provided
	// error is nil. The batch of intents may be resolved more efficiently than
	// if they were resolved individually.
	ResolveDeferredIntents(context.Context, kvpb.AdmissionHeader, []roachpb.LockUpdate) *Error
}

// txnWaitQueue holds a collection of wait-queues for transaction records.
// Conflicting transactions, known as "pushers", sit in a queue associated with
// an extant transaction that they conflict with, known as the "pushee", and
// wait for the pushee transaction to commit or abort.
//
// Typically, waiting for a pushee's transaction record to undergo a state
// transition is sufficient to satisfy a pusher transaction. Reacting to state
// transitions for conflicting transactions is powered by the TransactionManager
// interface.
//
// Just like with the lockTableWaiter, there are cases where reacting to state
// transitions alone in insufficient to make forward progress. However, unlike
// with the lockTableWaiter, the location of the txnWaitQueue on the range
// containing the conflicting transaction's record instead of on the range
// containing the conflicting transaction's lock presents an opportunity to
// actively resolve these situations. This is because a transaction's record
// reflects its authoritative status.
//
// The first of these situations is failure of the conflicting transaction's
// coordinator. This situation comes in two flavors:
//   - before a transaction has been finalized (committed or aborted)
//   - after a transaction has been finalized but before all of its intents have
//     been resolved
//
// In the first of these flavors, the transaction record may still have a
// PENDING status. Without a live transaction coordinator heartbeating it, the
// record will eventually expire and be abortable. The the second of these
// flavors, the transaction's record will already be committed or aborted.
// Regardless of which case the push falls into, once the transaction record
// is observed in a finalized state, the push will succeed, kick off intent
// resolution, and return to the sender.
//
// The second of these situations is transaction deadlock. Deadlocks occur when
// the lock acquisition patterns of two or more transactions interact in such a
// way that a cycle emerges in the "waits-for" graph of transactions. To break
// this cycle, one of the transactions must be aborted or it is impossible for
// any of the transactions that are part of the deadlock to continue making
// progress.
//
// The txnWaitQueue provides a mechanism for detecting these cycles across a
// distributed graph of transactions. Distributed deadlock detection works by
// having each pusher transaction that is waiting in the queue for a different
// transaction periodically query its own record using a QueryTxn request. While
// on the pusher's own transaction record range, the QueryTxn request uses the
// GetDependents method to collect the IDs of all locally-known transactions
// that are waiting for the pusher itself to release its locks. Of course, this
// local view of the dependency graph is incomplete, as it does not initially
// take into consideration transitive dependencies. To address this, when the
// QueryTxn returns to the initial txnWaitQueue, the pusher records its own
// dependencies as dependencies of its pushee transaction. As this process
// continues and pushers periodically query for their own dependencies and
// transfer these to their pushee, each txnWaitQueue accumulate more information
// about the global "waits-for" graph. Eventually, one of the txnWaitQueues is
// able to observe a full cycle in this graph and aborts one of the transactions
// in the cycle to break the deadlock.
//
// # Example of Distributed Deadlock Detection
//
// The following diagram demonstrates how the txnWaitQueue interacts with
// distributed deadlock detection.
//
//   - txnA enters txnB's txnWaitQueue during a PushTxn request (MaybeWaitForPush)
//
//   - txnB enters txnC's txnWaitQueue during a PushTxn request (MaybeWaitForPush)
//
//   - txnC enters txnA's txnWaitQueue during a PushTxn request (MaybeWaitForPush)
//
//     .-----------------------------------.
//     |                                   |
//     v                                   |
//     [txnA record] --> [txnB record] --> [txnC record]
//     deps:             deps:             deps:
//
//   - txnC            - txnA            - txnB
//
//   - txnA queries its own txnWaitQueue using a QueryTxn request (MaybeWaitForQuery)
//
//     .-----------------------------------.
//     |    ............                   |
//     v    v          .                   |
//     [txnA record] --> [txnB record] --> [txnC record]
//     deps:             deps:             deps:
//
//   - txnC            - txnA            - txnB
//
//   - txnA finds that txnC is a dependent. It transfers this dependency to txnB
//
//     .-----------------------------------.
//     |                                   |
//     v                                   |
//     [txnA record] --> [txnB record] --> [txnC record]
//     deps:             deps:             deps:
//
//   - txnC            - txnA            - txnB
//
//   - txnC
//
//   - txnC queries its own txnWaitQueue using a QueryTxn request (MaybeWaitForQuery)
//
//   - txnB queries its own txnWaitQueue using a QueryTxn request (MaybeWaitForQuery)
//
//   - txnC finds that txnB is a dependent. It transfers this dependency to txnA
//
//   - txnB finds that txnA and txnC are dependents. It transfers these dependencies to txnC
//
//     .-----------------------------------.
//     |                                   |
//     v                                   |
//     [txnA record] --> [txnB record] --> [txnC record]
//     deps:             deps:             deps:
//
//   - txnC            - txnA            - txnB
//
//   - txnB            - txnC            - txnA
//
//   - txnC
//
//   - txnB notices that txnC is a transitive dependency of itself. This indicates
//     a cycle in the global wait-for graph. txnC is aborted, breaking the cycle
//     and the deadlock
//
//     [txnA record] --> [txnB record] --> [txnC record: ABORTED]
//
//   - txnC releases its locks and the transactions proceed in order.
//
//     [txnA record] --> [txnB record] --> (free to commit)
//
// TODO(nvanbenschoten): if we exposed a "queue guard" interface, we could make
// stronger guarantees around cleaning up enqueued txns when there are no
// waiters.
type txnWaitQueue interface {
	requestQueuer

	// EnqueueTxn creates a queue associated with the provided transaction. Once
	// a queue is established, pushers of this transaction can wait in the queue
	// and will be informed of state transitions that the transaction undergoes.
	EnqueueTxn(*roachpb.Transaction)

	// UpdateTxn informs the queue that the provided transaction has undergone
	// a state transition. This will be communicated to any waiting pushers.
	UpdateTxn(context.Context, *roachpb.Transaction)

	// GetDependents returns a set of transactions waiting on the specified
	// transaction either directly or indirectly. The method is used to perform
	// deadlock detection.
	GetDependents(uuid.UUID) []uuid.UUID

	// MaybeWaitForPush checks whether there is a queue already established for
	// transaction being pushed by the provided request. If not, or if the
	// PushTxn request isn't queueable, the method returns immediately. If there
	// is a queue, the method enqueues this request as a waiter and waits for
	// the transaction to be pushed/finalized.
	//
	// If the transaction is successfully pushed while this method is waiting,
	// the first return value is a non-nil PushTxnResponse object.
	MaybeWaitForPush(context.Context, *kvpb.PushTxnRequest, lock.WaitPolicy) (*kvpb.PushTxnResponse, *Error)

	// MaybeWaitForQuery checks whether there is a queue already established for
	// transaction being queried. If not, or if the QueryTxn request hasn't
	// specified WaitForUpdate, the method returns immediately. If there is a
	// queue, the method enqueues this request as a waiter and waits for any
	// updates to the target transaction.
	MaybeWaitForQuery(context.Context, *kvpb.QueryTxnRequest) *Error

	// OnRangeDescUpdated informs the Queue that its range's descriptor has been
	// updated.
	OnRangeDescUpdated(*roachpb.RangeDescriptor)
}

// requestQueuer queues requests until some condition is met.
type requestQueuer interface {
	// Enable allows requests to be queued. The method is idempotent.
	// The lease sequence is used to avoid mixing incompatible requests
	// or other state from different leasing periods.
	Enable(roachpb.LeaseSequence)

	// Clear empties the queue(s) and causes all waiting requests to
	// return. If disable is true, future requests must not be enqueued.
	Clear(disable bool)

	// ClearGE empties the queue(s) for any keys greater or equal
	// to than the given key.
	ClearGE(roachpb.Key) []roachpb.LockAcquisition
}
