//go:build tools
// +build tools

/*
 * SPDX-FileCopyrightText: Contributors to the Gardener project
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package tools

import (
	_ "github.com/golang/mock/mockgen"
	_ "golang.org/x/tools/cmd/goimports"
)
