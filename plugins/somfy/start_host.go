//go:build !tamago

package main

import "github.com/xinix00/stulp/internal/appsdk"

// start op een host: Stulp heeft dit proces gestart en één kant van een
// socketpair als fd 3 meegegeven, of het proces meldt zich zelf aan op de
// socket of poort uit STULP_ATTACH. appsdk.Run kiest dat zelf.
func start(p appsdk.Plugin) { appsdk.Run(p) }
