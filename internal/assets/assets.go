// Package assets embeds the files the Controller ships: the default Runner
// Image Dockerfile and the entrypoint it copies into every Runner Container.
package assets

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
)

//go:embed runner.Dockerfile
var RunnerDockerfile []byte

// RecipeLabel is the image label holding the RunnerRecipe a default Runner
// Image was built from.
const RecipeLabel = "scaleset-runner.recipe"

// RunnerRecipe identifies the content of RunnerDockerfile. A default Runner
// Image with another recipe was built by a Controller version with a
// different Dockerfile and is rebuilt at startup.
var RunnerRecipe = func() string {
	sum := sha256.Sum256(RunnerDockerfile)
	return hex.EncodeToString(sum[:])[:16]
}()

//go:embed entrypoint.sh
var Entrypoint []byte

// ReadyMarker is the line the entrypoint prints once the runner listens.
const ReadyMarker = "SCALESET-RUNNER-READY-7f3c1e"
