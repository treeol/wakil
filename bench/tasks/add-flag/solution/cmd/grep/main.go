// Command grep prints lines containing a fixed string.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func main() {
	number := false
	var args []string
	for _, a := range os.Args[1:] {
		switch a {
		case "-n":
			number = true
		default:
			if strings.HasPrefix(a, "-") && a != "-" {
				fmt.Fprintf(os.Stderr, "unknown flag: %s\nusage: grep [-n] <string>\n", a)
				os.Exit(3)
			}
			args = append(args, a)
		}
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: grep [-n] <string>")
		os.Exit(3)
	}
	needle := args[0]
	sc := bufio.NewScanner(os.Stdin)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if strings.Contains(line, needle) {
			if number {
				fmt.Printf("%d:%s\n", lineNo, line)
			} else {
				fmt.Println(line)
			}
		}
	}
}
