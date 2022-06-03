---
name: Bug report
about: Let us know about a bug
title: ''
labels: bug
assignees: ''

---

**Describe the bug**

As clearly as you can, please tell us what the bug is.

**To Reproduce**

Help us to reproduce the buggy behavior. Ideally, you'd provide a
self-contained test that shows us the bug:

```bash
mkdir tmp && cd ./tmp
go mod init example
go get github.com/bufbuild/connect-go
touch example_test.go
```

And in `example_test.go`:

```go
package bugreport

func TestThatReproducesBug(t *testing.T) {
  // your reproduction here
}
```

**Environment (please complete the following information):**
- `connect-go` version or commit: (for example, `v0.1.0` or `5bfc7a1b440ebffdc952d813332e3617ca611395`)
- `go version`: (for example, `go version go1.18.3 darwin/amd64`)
- your complete `go.mod`:

```go
```

**Additional context**
Add any other context about the problem here.
