module github.com/dest1256/d256-node

go 1.27.3

require github.com/dest1256/d256-panel/proto v0.0.0

require (
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1 // indirect
	google.golang.org/grpc v1.85.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

// Local development against the sibling checkout. Replaced by a tagged
// version (proto/vX.Y.Z) before a release; see docs/PROTO.md.
replace github.com/dest1256/d256-panel/proto => ../d256-panel/proto
