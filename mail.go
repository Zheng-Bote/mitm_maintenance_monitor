/**
 * SPDX-FileComment: MitM Maintenance Monitor - mail files
 * SPDX-FileType: SOURCE
 * SPDX-FileContributor: ZHENG Robert
 * SPDX-FileCopyrightText: 2026 ZHENG Robert
 * SPDX-License-Identifier: Apache-2.0
 *
 * @file mail.go
 * @brief Renders mails in the header-block format of mitm_maintenance_sendmail and writes them atomically.
 * @version 0.1.0
 * @date 2026-11-10
 *
 * @author ZHENG Robert (robert@hase-zheng.net)
 * @copyright Copyright (c) 2026 ZHENG Robert
 * @license Apache-2.0
 */

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	mailFileSuffix = ".SendMail"
	tempFileSuffix = ".tmp"
)

var lineBreaks = regexp.MustCompile(`[\r\n]+`)

// headerValue removes line breaks so a value cannot inject further headers.
func headerValue(value string) string {
	return strings.TrimSpace(lineBreaks.ReplaceAllString(value, " "))
}

// render returns the mail file content: header block, empty line, body.
func render(mail Mail) []byte {
	var out strings.Builder
	fmt.Fprintf(&out, "To: %s\n", headerValue(strings.Join(mail.To, ", ")))
	if len(mail.Cc) > 0 {
		fmt.Fprintf(&out, "Cc: %s\n", headerValue(strings.Join(mail.Cc, ", ")))
	}
	fmt.Fprintf(&out, "Subject: %s\n", headerValue(mail.Subject))
	fmt.Fprintf(&out, "X-MitM-Severity: %s\n", headerValue(mail.Severity))
	fmt.Fprintf(&out, "X-MitM-Category: %s\n", headerValue(mail.Category))
	fmt.Fprintf(&out, "X-MitM-Source: %s\n", headerValue(truncate(strings.Join(mail.Sources, ", "), 900)))
	out.WriteString("\n")
	out.WriteString(mail.Body)
	fmt.Fprintf(&out, "\n--\n%s %s\n", appName, version)
	return []byte(out.String())
}

// writeMail writes <dir>/<name>.tmp with mode 0600 and renames it to <dir>/<name>.SendMail,
// so mitm_maintenance_sendmail never reads a half-written file.
func writeMail(dir string, mail Mail) error {
	temp := filepath.Join(dir, mail.Name+tempFileSuffix)
	target := filepath.Join(dir, mail.Name+mailFileSuffix)
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create mail file %s: %w", temp, err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		os.Remove(temp)
		return fmt.Errorf("set mode of mail file %s: %w", temp, err)
	}
	if _, err := file.Write(render(mail)); err != nil {
		file.Close()
		os.Remove(temp)
		return fmt.Errorf("write mail file %s: %w", temp, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(temp)
		return fmt.Errorf("sync mail file %s: %w", temp, err)
	}
	if err := file.Close(); err != nil {
		os.Remove(temp)
		return fmt.Errorf("close mail file %s: %w", temp, err)
	}
	if err := os.Rename(temp, target); err != nil {
		os.Remove(temp)
		return fmt.Errorf("rename mail file to %s: %w", target, err)
	}
	return nil
}
