# Coordinated Node Release Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Publish the exit node and the Compose Multiplatform clients from an immutable, shared core tag.

**Architecture:** `meepo161/openfluxfork` owns the version and invokes the client repository's existing dispatchable release workflow with its immutable `vX.Y.Z` core reference. The client workflow builds all desktop packages and a signed Android APK from that ref; only once that workflow succeeds does the core workflow publish its exit-node release.

**Tech Stack:** GitHub Actions, Go cross-compilation, Gradle/Compose Multiplatform, gomobile, Android SDK/NDK.

---

### Task 1: Add the shared-release trigger contract in the core repository

**Files:**
- Create: `.github/workflows/client-node-release.yml`
- Test: `.github/workflows/client-node-release.yml` validated with `actionlint`

**Step 1: Write the failing workflow contract check**

Create a temporary `actionlint` invocation that requires the new workflow to expose `v*` tags, use `CLIENT_RELEASE_TOKEN`, dispatch `meepo161/openfluxfordesktop`, and pass `version`, `core_ref`, and `publish` inputs.

**Step 2: Run the check to verify it fails**

Run: `actionlint .github/workflows/client-node-release.yml`

Expected: fail because the workflow does not exist.

**Step 3: Implement the minimal core workflow**

Create a workflow that:

1. triggers on `client-v*` tags and manual dispatch with a semver version;
2. validates the version and resolves the tagged core SHA;
3. invokes the client workflow with `gh workflow run release.yml --repo meepo161/openfluxfordesktop --ref <client ref>` and inputs `version`, `core_ref=client-vX.Y.Z`, and `publish=true`;
4. waits for the dispatched run and fails on a non-success conclusion;
5. only then builds the Linux `exitnode` artifacts and publishes release assets with the shared version.

Use `CLIENT_RELEASE_TOKEN` solely for the cross-repository `gh` calls and the default `GITHUB_TOKEN` for publishing the core repository's own release.

**Step 4: Run the check to verify it passes**

Run: `actionlint .github/workflows/client-node-release.yml`

Expected: workflow parses without errors.

**Step 5: Commit**

```bash
git add .github/workflows/client-node-release.yml
git commit -m "ci: coordinate client and node releases"
```

### Task 2: Extend the Compose client release workflow with Android packaging

**Files:**
- Modify: `C:/Users/meepo/openflux/OpenFluxDesktop/.github/workflows/release.yml`
- Test: `C:/Users/meepo/openflux/OpenFluxDesktop/.github/workflows/release.yml` validated with `actionlint`

**Step 1: Write the failing workflow contract check**

Check that the workflow has an Android build job that checks out `${{ env.CORE_REPO }}@${{ env.CORE_REF }}`, runs `scripts/build-android-core.sh .core`, assembles signed release APKs, and uploads them as release artifacts.

**Step 2: Run the check to verify it fails**

Run: `actionlint .github/workflows/release.yml`

Expected: the lint command succeeds structurally but the new contract assertions fail because no Android job exists.

**Step 3: Implement the Android build job**

Add an Ubuntu job parallel to the desktop matrix which:

1. checks out the Compose client and specified core reference;
2. installs Go from `.core/go.mod`, Java 17, Android SDK platform/build-tools/NDK 27, Gradle, and the pinned gomobile tool;
3. restores the release keystore from repository secrets;
4. calls `scripts/build-android-core.sh .core` to generate `androidApp/libs/openflux.aar` and `openflux-core.version`;
5. runs `:shared:jvmTest` and `:androidApp:assembleRelease -PappVersion=<version>`;
6. verifies the APK signatures and collects the ABI and universal APKs as `OpenFlux-<version>-android-*.apk`;
7. uploads those artifacts.

Update the publish job to download the Android artifacts alongside desktop artifacts, include them in `SHA256SUMS.txt`, and list Android installation guidance in the release body.

**Step 4: Run the check to verify it passes**

Run: `actionlint .github/workflows/release.yml`

Expected: parses without errors; the contract assertions find the Android build and artifact paths.

**Step 5: Commit**

```bash
git -C C:/Users/meepo/openflux/OpenFluxDesktop add .github/workflows/release.yml
git -C C:/Users/meepo/openflux/OpenFluxDesktop commit -m "ci: publish Android Compose client"
```

### Task 3: Document release operation and verify dispatch safety

**Files:**
- Modify: `README.ru.md`
- Modify: `README.md`
- Modify: `C:/Users/meepo/openflux/OpenFluxDesktop/README.md`
- Test: workflow manual dispatch without `publish`

**Step 1: Write the failing documentation check**

Search the three READMEs for `CLIENT_RELEASE_TOKEN`, `client-vX.Y.Z`, and the explanation that `core_ref` is immutable.

**Step 2: Run the check to verify it fails**

Run: `rg -n "CLIENT_RELEASE_TOKEN|client-vX.Y.Z|core_ref" README.md README.ru.md C:/Users/meepo/openflux/OpenFluxDesktop/README.md`

Expected: missing required operational guidance.

**Step 3: Document the release procedure**

Document the required fine-grained secret, tag format, artifact ownership, rollback expectations, and a non-publishing client workflow dispatch using a core tag.

**Step 4: Run the documentation and workflow verification**

Run:

```bash
actionlint .github/workflows/client-node-release.yml
actionlint C:/Users/meepo/openflux/OpenFluxDesktop/.github/workflows/release.yml
rg -n "CLIENT_RELEASE_TOKEN|client-vX.Y.Z|core_ref" README.md README.ru.md C:/Users/meepo/openflux/OpenFluxDesktop/README.md
```

Expected: both workflows lint cleanly and all operational markers appear.

**Step 5: Commit**

```bash
git add README.md README.ru.md docs/plans/2026-09-27-coordinated-node-release.md
git commit -m "docs: explain coordinated node releases"
git -C C:/Users/meepo/openflux/OpenFluxDesktop add README.md
git -C C:/Users/meepo/openflux/OpenFluxDesktop commit -m "docs: explain coordinated client releases"
```
