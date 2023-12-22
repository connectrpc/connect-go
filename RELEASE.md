# Release Process for `connect-go`

## Preparing for Release

1. **Update Version Number**
   - In `connect.go`, modify the `Version` constant to match the target release according to [semver](https://semver.org/) standards. Remove the `-dev` suffix.
     ```go
     const Version = "X.Y.Z"
     ```

2. **Version Requirements (Optional)**
   - If changes require a new API in `connect-go`, declare a constant referencing the updated code.
     ```go
     const (
         IsAtLeastVersionX_Y_Z = true
     )
     ```
   - Ensure the generated code refers to this constant. This is used as a compile time check of source compatibilty.

3. **Branch and PR Creation**
   - Create a branch, commit the version changes, and open a PR titled "Prepare for vX.Y.Z".
   - Merge this PR into the main branch. No other changes should be merged until the release is complete.

## Creating the Release

1. **Draft a Release**
   - Navigate to the [releases](https://github.com/connectrpc/connect-go/releases) page using the GitHub UI.
   - Click **Draft a new release** at the top of the page.

2. **Tagging and Title**
   - Enter “vX.Y.Z” in the "Choose a tag” field to create a new tag for the release upon publishing.
   - Target the main branch and title the release as “vX.Y.Z”.

3. **Additional Configuration**
   - Click “set as latest release” and specify the last version as the “Previous tag”.
   - Use **Generate release notes** to auto-create meaningful release notes, sorting them into `### Enhancements` and `### Bugfixes`. Ensure contributors are credited, especially new external contributors.

4. **Publishing the Release**
   - When ready, click **Publish release**.
   - For proper sorting, edit and update the newly created release. See this [issue](https://github.com/orgs/community/discussions/8226) for guidelines.

## Returning to Development

1. **Update for Development**
   - In `connect.go`, modify the `Version` constant to the next minor release with the suffix `-dev`.
     ```go
     const Version = "X.Y+1.0-dev"
     ```
   - Use the next minor release. Bugs and patch releases aren't used.

2. **PR Creation**
   - Open a PR with the title "Back to development".
