# Releasing connect-go

This document outlines how to create a release of connect-go.

1. Clone the repo, ensuring you have the latest main.

2. On a new branch, open [connect.go](connect.go) and change the `Version` constant. Do not just remove the `-dev` suffix: look at the release history and the unreleased commits to choose a new semantic version number. Example: [#661](https://github.com/connectrpc/connect-go/pull/661)
```patch
-const Version = "1.14.0-dev"
+const Version = "1.14.0"
```

3. Open a PR titled "Prepare for vX.Y.Z". Once it's reviewed and CI passes, merge it. *Make sure no new commits are added to merged until the release is complete.*

4. Using the Github UI, create a new release.
    - Under “Choose a tag”, type in “vX.Y.Z” to create a new tag for the release upon publish.
    - Target the main branch.
    - Title the Release “vX.Y.Z”.
    - Click “set as latest release”.
    - Set the last version as the “Previous tag”.
    - Click “Generate release notes” to autogenerate release notes, sort them into ### Enhancements and ### Bugfixes, and edit the PR titles to be meaningful to end users. Feel free to collect multiple small changes to docs or Github config into one line, but try to tag every contributor. Make especially sure to credit new external contributors!

5. Publish the release.

6. Take the newly created release, click on the button to edit the release, and then update the release. See this [issue](https://github.com/orgs/community/discussions/8226) for guidelines.

7. On a new branch, open [connect.go](connect.go) and change the `Version` to increment the minor tag and append the `-dev` suffix. Use the next minor release - we never anticipate bugs and patch releases. Example: [#662](https://github.com/connectrpc/connect-go/pull/662)
```patch
-const Version = "1.14.0"
+const Version = "1.15.0-dev"
```

8. Open a PR titled "Back to developement". Once it's reviewed and CI passes, merge it. The release is complete.

