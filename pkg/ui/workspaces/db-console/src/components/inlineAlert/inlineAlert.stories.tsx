// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

import { storiesOf } from "@storybook/react";
import React from "react";

import { Anchor } from "src/components";
import { styledWrapper } from "src/util/decorators";

import { InlineAlert } from "./inlineAlert";

storiesOf("InlineAlert", module)
  .addDecorator(styledWrapper({ padding: "24px" }))
  .add("with text title", () => (
    <InlineAlert title="Hello world!" message="blah-blah-blah" />
  ))
  .add("with Error intent", () => (
    <InlineAlert title="Hello world!" message="blah-blah-blah" intent="error" />
  ))
  .add("with link in title", () => (
    <InlineAlert
      title={
        <span>
          You do not have permission to view this information.{" "}
          <Anchor href="#">Learn more.</Anchor>
        </span>
      }
    />
  ))
  .add("with multiline message", () => (
    <InlineAlert
      title="Hello world!"
      message={
        <div>
          <div>Message 1</div>
          <div>Message 2</div>
          <div>Message 3</div>
        </div>
      }
    />
  ));
