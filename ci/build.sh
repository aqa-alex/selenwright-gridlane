#!/bin/bash

set -e

export GO111MODULE="on"

# Snapshot cross-compile for linux/darwin × amd64/arm64 into dist/.
# Release builds are driven directly by goreleaser in release.yml.
goreleaser build --snapshot --clean
