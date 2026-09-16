package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

var stdin = bufio.NewReader(os.Stdin)

func promptString(label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, _ := stdin.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func promptChoice(label string, choices []string, def string) string {
	for {
		v := promptString(fmt.Sprintf("%s (%s)", label, strings.Join(choices, "/")), def)
		for _, c := range choices {
			if c == v {
				return v
			}
		}
		fmt.Printf("Please choose one of: %s\n", strings.Join(choices, ", "))
	}
}

func promptInt(label string, def int) int {
	v := promptString(label, strconv.Itoa(def))
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func confirmPrompt(label string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	v := strings.ToLower(promptString(fmt.Sprintf("%s [%s]", label, hint), ""))
	if v == "" {
		return def
	}
	return v == "y" || v == "yes"
}
