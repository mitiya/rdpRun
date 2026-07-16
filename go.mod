module github.com/mitiya/rdprun

go 1.25.5

require github.com/tomatome/grdp v0.1.0

require (
	github.com/huin/asn1ber v0.0.0-20120622192748-af09f62e6358 // indirect
	github.com/icodeface/tls v0.0.0-20190904083142-17aec93c60e5 // indirect
	github.com/lunixbochs/struc v0.0.0-20200707160740-784aaebc1d40 // indirect
	golang.org/x/crypto v0.0.0-20220622213112-05595931fe9d // indirect
)

// Use a local, cgo-free patched copy of tomatome/grdp so the project can be
// cross-compiled for Linux without a C cross-compiler. The only change in the
// vendored copy is removing the vestigial `import "C"` from plugin/channel.go.
replace github.com/tomatome/grdp => ./third_party/grdp
