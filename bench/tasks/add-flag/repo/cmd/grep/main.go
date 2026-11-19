// Command grep prints lines containing a fixed string.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: grep <string>")
		os.Exit(3)
	}
	needle := os.Args[1]
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, needle) {
			fmt.Println(line)
		}
	}
}
