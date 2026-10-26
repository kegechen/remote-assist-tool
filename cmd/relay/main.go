// Package main is the public relay entry point. Build-time module integrations
// use the same relayapp.Main entry point after importing their built-in modules.
package main

import "github.com/remote-assist/tool/pkg/relayapp"

func main() { relayapp.Main() }
