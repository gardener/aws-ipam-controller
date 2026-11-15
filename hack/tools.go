//go:build tools
// +build tools

// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

// This package imports things required by build scripts, to force `go mod` to see them as dependencies
package tools

import (
	_ "github.com/gardener/gardener/cmd/utils" // keep github.com/gardener/gardener in go.mod for GARDENER_HACK_DIR in Makefile
	_ "github.com/gardener/gardener/hack/tools"
	_ "github.com/gardener/gardener/pkg/apis"
)
