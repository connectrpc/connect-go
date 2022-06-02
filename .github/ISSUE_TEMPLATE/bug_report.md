---
name: Bug report
about: Let us know about a bug you have found
title: ''
labels: bug
assignees: ''

---

**Describe the bug**
Provide us with clear and concise description of what the bug is.

**To Reproduce**
Provide us with specific steps to reproduce the behavior. In most cases, we should be able to copy and paste these steps to reproduce the issue you are seeing. In a perfect world, this would look like:
```go
mkdir tmp
cd ./tmp
go mod init example
go get github.com/bufbuild/connect-go
cat <<EOF > bug_report_test.go
package bugreport

func TestThatReproducesBug(t *testing.T) {
  // your reproduction here
}
```

**Environment (please complete the following information):**
- `connect-go` version or commit: [e.g. `v0.1.0` or `5bfc7a1b440ebffdc952d813332e3617ca611395`]
 - `go version`: [e.g. `go version go1.18.3 darwin/amd64`]

**Additional context**
Add any other context about the problem here.
