// A separate child executable for process/terminal lifecycle tests. It is not a
// shell and never re-enters the Go test binary or implements shell commands.
package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"

	"golang.org/x/term"
)

func main() {
	if len(os.Args) != 3 || os.Args[1] != "-c" {
		fmt.Fprintln(os.Stderr, "unexpected child arguments")
		os.Exit(99)
	}
	switch os.Args[2] {
	case "environment":
		fmt.Print(os.Getenv("OUTPOST_TEST_VALUE"))
		os.Exit(42)
	case "tree":
		child := exec.Command(os.Args[0], "-c", "listener")
		child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(97)
		}
		fmt.Printf("descendant=%d\n", child.Process.Pid)
		_ = child.Wait()
	case "listener":
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(96)
		}
		fmt.Printf("child-listener=%s\n", ln.Addr())
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	case "wait":
		fmt.Println("child-ready")
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			if scanner.Text() == "size" {
				// Stdout is the console output buffer on Windows (CONOUT$),
				// whose window ConPTY resizes. Stdin is the input buffer,
				// which has no screen geometry; on Unix both are the PTY.
				cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
				if err != nil {
					fmt.Println(err)
				} else {
					fmt.Printf("size=%dx%d\n", cols, rows)
				}
			}
		}
	default:
		os.Exit(98)
	}
}
