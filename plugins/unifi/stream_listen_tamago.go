//go:build tamago

package main

import "net"

// streamListen op een node: er ís geen localhost — leannet bezit één adres,
// het slot-IP, en Stulp haalt het beeld dáár op (fetchimage_tamago doet een
// kale http-GET over de switch). De wildcard-bind levert dat adres, en
// listener.Addr() zet het vanzelf in de stream-URL. De grens is het interne
// switch-netwerk zelf: precies wat HOP per slot isoleert, dezelfde afweging
// als het beeld-ophaalpad aan Stulps kant maakt.
func streamListen() (net.Listener, error) {
	return net.Listen("tcp", ":0")
}
