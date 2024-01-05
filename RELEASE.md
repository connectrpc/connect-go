# Releasing connect-go

This document outlines how to create a release of connect-go.

1. Clone the repo, ensuring you have the latest main.

2. On a new branch, open [connect.go](connect.go) and change the `Version` constant to an appropriate [semantic version](https://semver.org/). To select the correct version, look at the version number of the [latest release](https://github.com/connectrpc/connect-go/releases/latest) and the changes that are included in this new release.
  * If there are only bug fixes and no new features, remove the `-dev` suffix, set MINOR number to be equal to the [latest release](https://github.com/connectrpc/connect-go/releases/latest), and set the PATCH number to be 1 more than the PATCH number of the [latest release](https://github.com/connectrpc/connect-go/releases/latest).
  * If there are features being released, remove the `-dev` suffix, set the MINOR number to be 1 more than the MINOR number of the [latest release](https://github.com/connectrpc/connect-go/releases/latest), and set the PATCH number to `0`. In the common case, the diff here will just be to remove the `-dev` suffix.

   ```patch
   -const Version = "1.14.0-dev"
   +const Version = "1.14.0"
   ```

3. Check [cmd/protoc-gen-connect-go/main.go](cmd/protoc-gen-connect-go/main.go) to see if a new `IsAtLeastVersionX_Y_Z` constant was added since the [latest release](https://github.com/connectrpc/connect-go/releases/latest). If one was added (this is rare), ensure that the version number matches the version number that you are currently releasing. If it doesn't match, update the name to match the version you are about to release. [Example PR #496](https://github.com/connectrpc/connect-go/pull/496).

4. Open a PR titled "Prepare for vX.Y.Z" ([Example PR #661](https://github.com/connectrpc/connect-go/pull/661)). Once it's reviewed and CI passes, merge it.

    *Make sure no new commits are merged until the release is complete.*

5. Using the Github UI, create a new release.
    - Under “Choose a tag”, type in “vX.Y.Z” to create a new tag for the release upon publish.
    - Target the main branch.
    - Title the Release “vX.Y.Z”.
    - Click “set as latest release”.
    - Set the last version as the “Previous tag”.
    - Click “Generate release notes” to autogenerate release notes, sort them into ### Enhancements and ### Bugfixes, and edit the PR titles to be meaningful to end users. Feel free to collect multiple small changes to docs or Github config into one line, but try to tag every contributor. Make especially sure to credit new external contributors!

6. Publish the release.

7. On a new branch, open [connect.go](connect.go) and change the `Version` to increment the minor tag and append the `-dev` suffix. Use the next minor release - we never anticipate bugs and patch releases.

   ```patch
   -const Version = "1.14.0"
   +const Version = "1.15.0-dev"
   ```

8. Open a PR titled "Back to development" ([Example PR #662](https://github.com/connectrpc/connect-go/pull/662)). Once it's reviewed and CI passes, merge it.

9. Check the [releases](https://github.com/connectrpc/connect-go/releases) page to see if [releases are out of order](https://github.com/orgs/community/discussions/8226). If they are, take the release you just did, click on the button to edit the release, and then update the release. If that doesn't work, [contact GitHub support](https://support.github.com/contact?tags=rr-general-technical) to request that they trigger a re-index of the repository:

   > Subject: connect-go releases appearing out of order
   >
   > The [connect-go releases page](https://github.com/connectrpc/connect-go/releases) is showing releases out of order. I have tried editing the most recent release to trigger a re-index and it doesn't seem to have resolved the issue. Can you please trigger a re-index for this repo? Thanks!