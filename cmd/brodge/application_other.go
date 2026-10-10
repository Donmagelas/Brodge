//go:build !darwin || !cgo

package main

func runApplication(path string) error { return runWithBrowser(path, true) }
