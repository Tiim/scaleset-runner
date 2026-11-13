// Package assets embeds the files the Controller ships: the default Runner
// Image Dockerfile and the entrypoint it copies into every Runner Container.
package assets

import _ "embed"

//go:embed runner.Dockerfile
var RunnerDockerfile []byte

//go:embed entrypoint.sh
var Entrypoint []byte

// ReadyMarker is the line the entrypoint prints once the runner listens.
const ReadyMarker = "SCALESET-RUNNER-READY-7f3c1e"
