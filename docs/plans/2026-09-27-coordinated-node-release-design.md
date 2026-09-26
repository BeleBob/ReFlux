# Coordinated Node Release Design

## Goal

Release the OpenFlux exit node and the Compose Multiplatform client from the
same version of `meepo161/openfluxfork`.

## Scope

The core repository (`meepo161/openfluxfork`) is the source of truth for a
release version and core revision. Its release workflow publishes the Linux
exit-node binaries, then starts the client repository's release workflow.

The client repository (`meepo161/openfluxfordesktop`) checks out that exact
core tag, bundles it into Windows, Linux and macOS packages, builds the Android
APK from its `androidApp` module, and publishes all client artifacts under the
same version.

The legacy `damnurmum/OpenFlux-Android` repository is not part of this release
path.

## Trigger and Versioning

A shared release uses a `vX.Y.Z` tag in the core repository. The core workflow
derives `X.Y.Z` from that tag and uses the immutable tag as the client's
`core_ref`. It triggers the client workflow through `workflow_dispatch` with
`version=X.Y.Z`, `core_ref=vX.Y.Z`, and `publish=true`.

The existing `node-v*` workflow remains unchanged for backward compatibility.
It continues to publish only the installer-pinned exit-node artifacts.

## Failure Handling

The core job waits for the dispatched client workflow to finish and fails if
the client workflow fails. GitHub releases are immutable enough in practice
that a later failure cannot safely retract a published exit-node release; the
workflow therefore publishes the exit-node release only after the client build
has completed successfully, and documents the common version on both releases.

Cross-repository dispatch requires a fine-grained GitHub token stored as
`CLIENT_RELEASE_TOKEN` in the core repository. It must have Actions write
access to `meepo161/openfluxfordesktop`; the client repository's default
`GITHUB_TOKEN` publishes its own release.

## Verification

Workflow validation covers the tag contract, dispatch inputs, and the Android
artifact paths. A manual non-publishing `workflow_dispatch` in the client repo
will verify that it builds the requested core reference. The first tagged
release is verified by matching the version and core revision recorded in the
two generated releases.
