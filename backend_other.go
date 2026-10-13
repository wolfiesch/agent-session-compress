//go:build !darwin && !linux && !windows

package main

func newBackend() Backend { return unsupportedBackend{} }
