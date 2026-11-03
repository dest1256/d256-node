# Protocol contract

The `.proto` files live in the panel repo (`d256-panel/proto`) and are published as the Go module
`github.com/dest1256/d256-panel/proto`, tagged `proto/vX.Y.Z`. Until the first tag exists, `go.mod` uses
`replace ... => ../d256-panel/proto` (sibling checkouts; `./dev` mounts the parent directory).

Before a release: delete the `replace`, `go get github.com/dest1256/d256-panel/proto@vX.Y.Z`, run `make verify`.
The node reports its protocol version in `Hello`; the panel rejects versions below its minimum.
