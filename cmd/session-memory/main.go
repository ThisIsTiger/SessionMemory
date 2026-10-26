// Command session-memory is the SessionMemory CLI. It only parses arguments
// and prints; the work is done by internal packages.
package main

import "os"

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
