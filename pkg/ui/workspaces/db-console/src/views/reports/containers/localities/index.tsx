// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

import { Loading } from "@cockroachlabs/cluster-ui";
import isNil from "lodash/isNil";
import React from "react";
import { Helmet } from "react-helmet";
import { connect } from "react-redux";
import { RouteComponentProps, withRouter } from "react-router-dom";

import { refreshLocations, refreshNodes } from "src/redux/apiReducers";
import { CachedDataReducerState } from "src/redux/cachedDataReducer";
import {
  LocalityTier,
  LocalityTree,
  selectLocalityTree,
} from "src/redux/localities";
import {
  LocationTree,
  selectLocationsRequestStatus,
  selectLocationTree,
} from "src/redux/locations";
import { selectNodeRequestStatus } from "src/redux/nodes";
import { AdminUIState } from "src/redux/state";
import { getNodeLocalityTiers } from "src/util/localities";
import { findMostSpecificLocation, hasLocation } from "src/util/locations";
import "./localities.styl";

import { BackToAdvanceDebug } from "../util";

function formatCoord(coordinate: number) {
  return coordinate.toFixed(4);
}

function renderLocation(locations: LocationTree, tiers: LocalityTier[]) {
  const location = findMostSpecificLocation(locations, tiers);

  if (isNil(location)) {
    return "";
  }

  return `${formatCoord(location.latitude)}, ${formatCoord(
    location.longitude,
  )}`;
}

function renderLocalityTree(locations: LocationTree, tree: LocalityTree) {
  let rows: React.ReactNode[] = [];
  const leftIndentStyle = {
    paddingLeft: `${20 * tree.tiers.length}px`,
  };

  tree.nodes.forEach(node => {
    const tiers = getNodeLocalityTiers(node);

    rows.push(
      <tr>
        <td></td>
        <td>
          n{node.desc.node_id} @ {node.desc.address.address_field}
        </td>
        <td className="parent-location">{renderLocation(locations, tiers)}</td>
      </tr>,
    );
  });

  Object.keys(tree.localities).forEach(key => {
    Object.keys(tree.localities[key]).forEach(value => {
      const child = tree.localities[key][value];

      rows.push(
        <tr>
          <td>
            <span style={leftIndentStyle}>
              {key}={value}
            </span>
          </td>
          <td></td>
          <td
            className={
              hasLocation(locations, { key, value })
                ? "own-location"
                : "parent-location"
            }
          >
            {renderLocation(locations, child.tiers)}
          </td>
        </tr>,
      );

      rows = rows.concat(renderLocalityTree(locations, child));
    });
  });

  return rows;
}

interface LocalitiesProps {
  localityTree: LocalityTree;
  localityStatus: CachedDataReducerState<any>;
  locationTree: LocationTree;
  locationStatus: CachedDataReducerState<any>;
  refreshLocations: typeof refreshLocations;
  refreshNodes: typeof refreshNodes;
}

export class Localities extends React.Component<
  LocalitiesProps & RouteComponentProps,
  {}
> {
  componentDidMount() {
    this.props.refreshLocations();
    this.props.refreshNodes();
  }

  componentDidUpdate() {
    this.props.refreshLocations();
    this.props.refreshNodes();
  }

  render() {
    return (
      <div>
        <Helmet title="Localities | Debug" />
        <BackToAdvanceDebug history={this.props.history} />
        <section className="section">
          <h1 className="base-heading">Localities</h1>
        </section>
        <Loading
          loading={
            !this.props.localityStatus.data || !this.props.locationStatus.data
          }
          page={"localities"}
          error={[
            this.props.localityStatus.lastError,
            this.props.locationStatus.lastError,
          ]}
          render={() => (
            <section className="section">
              <table className="locality-table">
                <thead>
                  <tr>
                    <th>Localities</th>
                    <th>Nodes</th>
                    <th>Location</th>
                  </tr>
                </thead>
                <tbody>
                  {renderLocalityTree(
                    this.props.locationTree,
                    this.props.localityTree,
                  )}
                </tbody>
              </table>
            </section>
          )}
        />
      </div>
    );
  }
}

const mapStateToProps = (state: AdminUIState, _: RouteComponentProps) => ({
  // RootState contains declaration for whole state
  localityTree: selectLocalityTree(state),
  localityStatus: selectNodeRequestStatus(state),
  locationTree: selectLocationTree(state),
  locationStatus: selectLocationsRequestStatus(state),
});

const mapDispatchToProps = {
  refreshLocations,
  refreshNodes,
};

export default withRouter(
  connect(mapStateToProps, mapDispatchToProps)(Localities),
);
