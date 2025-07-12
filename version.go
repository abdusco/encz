package main

import "fmt"

var gitTag string = "dev"
var gitCommit string = "unknown"

var version string = func() string {
	return fmt.Sprintf("%s-%s", gitTag, gitCommit)
}()
