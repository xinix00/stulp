//go:build !tamago

package main

import "github.com/xinix00/stulp/internal/appsdk"

func start(p appsdk.Plugin) { appsdk.Run(p) }
