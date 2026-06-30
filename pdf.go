package main

import (
	"bytes"
	"fmt"
	"os/exec"
)

// pdfToText converts PDF bytes to plain text using pdftotext (poppler-utils).
// Falls back gracefully if pdftotext is not installed.
func pdfToText(data []byte) (string, error) {
	_, err := exec.LookPath("pdftotext")
	if err != nil {
		return "", fmt.Errorf("pdftotext not found: install poppler-utils")
	}
	cmd := exec.Command("pdftotext", "-layout", "-", "-")
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("pdftotext failed: %w", err)
	}
	return string(out), nil
}
