package tui

import "github.com/charmbracelet/x/ansi"

func widthOf(s string) int { return ansi.StringWidth(s) }
