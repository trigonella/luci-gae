gae: A Google AppEngine SDK wrapper designed for testing+extensibility (for Go)
===================

[![GoDoc](https://godoc.org/github.com/luci/gae?status.svg)](https://godoc.org/github.com/luci/gae)
[![Build Status](https://travis-ci.org/luci/gae.svg?branch=master)](https://travis-ci.org/luci/gae)
[![Coverage Status](https://coveralls.io/repos/luci/gae/badge.svg?branch=master&service=github)](https://coveralls.io/github/luci/gae?branch=master)

Installing
----------

    go get -u github.com/luci/gae/...

Why/What/How
------------

See the [godocs](https://godoc.org/github.com/luci/gae).

Versioning
----------

  * Branch `master` constains the latest code.

Contributing
------------

  * Sign the [Google CLA](https://cla.developers.google.com/clas).
  * Make sure your `user.email` and `user.name` are configured in `git config`.
  * Install the [pcg](https://github.com/maruel/pre-commit-go) git hook:
    `go get -u github.com/maruel/pre-commit-go/cmd/... && pcg`

Run the following to setup the code review tool and create your first review:

    git clone https://chromium.googlesource.com/chromium/tools/depot_tools.git $HOME/src/depot_tools
    export PATH="$PATH:$HOME/src/depot_tools"
    cd $GOROOT/github.com/luci/gae
    git new-branch work
    # or `git checkout -b work origin/master` if you like typing more.

    # hack hack

    git commit -a -m "This is awesome\nR=joe@example.com"
    # This will ask for your Google Account credentials.
    git cl upload -s
    # Wait for LGTM over email.
    git cl land

Use `git cl help` and `git cl help <cmd>` for more details.
