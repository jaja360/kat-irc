package main

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var versionFile string

// version is the release version, embedded from the VERSION file at build time.
var version = strings.TrimSpace(versionFile)
