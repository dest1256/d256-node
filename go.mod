module github.com/dest1256/d256-node

go 1.27.3

require (
	github.com/dest1256/d256-panel/proto v0.0.0
	github.com/shirou/gopsutil/v4 v4.26.10
	google.golang.org/grpc v1.86.0-dev
)

require (
	github.com/ebitengine/purego v0.11.2 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/lufia/plan9stats v0.0.0-20211012122336-39d0f177ccd0 // indirect
	github.com/power-devops/perfstat v0.0.0-20260805114148-88456608a4f6 // indirect
	github.com/tklauser/go-sysconf v0.4.0 // indirect
	github.com/tklauser/numcpus v0.12.0 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

// Local development against the sibling checkout. Replaced by a tagged
// version (proto/vX.Y.Z) before a release; see docs/PROTO.md.
replace github.com/dest1256/d256-panel/proto => ../d256-panel/proto
