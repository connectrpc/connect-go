Contributing
============

We'd love your help making Connect better!

If you'd like to add new exported APIs, please [open an issue][open-issue]
describing your proposal &mdash; discussing API changes ahead of time makes
pull request review much smoother. In your issue, pull request, and any other
communications, please remember to treat your fellow contributors with
respect!

Note that for a contribution to be accepted, you must sign off on all commits
in order to affirm that they comply with the [Developer Certificate of Origin][dco].
Make sure to configure `git` with the same name and E-Mail as your GitHub account,
and run `git commit` with the `-s` flag to sign. If necessary, a bot will remind
you to sign your commits when you open your pull request, and provide helpful tips.

## Setup

[Fork][fork], then clone the repository:

```
mkdir -p $GOPATH/src/connectrpc.com
cd $GOPATH/src/connectrpc.com
git clone git@github.com:your_github_username/connect-go.git connect
cd connect
git remote add upstream https://github.com/connectrpc/connect-go.git
git fetch upstream
```

Make sure that the tests and the linters pass (you'll need `bash` and the
latest stable Go release installed):

```
make 
```

## Making Changes

Start by creating a new branch for your changes:

```
cd $GOPATH/src/connectrpc.com/connect
git checkout main
git fetch upstream
git rebase upstream/main
git checkout -b cool_new_feature
```

Make your changes, then ensure that `make` still passes. (Unless you're
changing `protoc-gen-connect-go`, you can use the standard `go build ./...` and
`go test ./...` while you're coding.) When you're satisfied with your changes,
push them to your fork.

```
git commit -a
git push origin cool_new_feature
```

Then use the GitHub UI to open a pull request.

At this point, you're waiting on us to review your changes. We *try* to respond
to issues and pull requests within a few business days, and we may suggest some
improvements or alternatives. Once your changes are approved, one of the
project maintainers will merge them.

We're much more likely to approve your changes if you:

* Add tests for new functionality.
* Write a [good commit message][commit-message].
* Maintain backward compatibility.

[fork]: https://github.com/connectrpc/connect-go/fork
[open-issue]: https://github.com/connectrpc/connect-go/issues/new
[dco]: https://developercertificate.org
[commit-message]: http://tbaggery.com/2008/04/19/a-note-about-git-commit-messages.html
