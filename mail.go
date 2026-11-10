/**
 * SPDX-FileComment: MitM Maintenance Monitor - mail files
 * SPDX-FileType: SOURCE
 * SPDX-FileContributor: ZHENG Robert
 * SPDX-FileCopyrightText: 2026 ZHENG Robert
 * SPDX-License-Identifier: Apache-2.0
 *
 * @file mail.go
 * @brief Renders mails in the header-block format of mitm_maintenance_sendmail and writes them atomically.
 * @version 0.3.0
 * @date 2026-11-10
 *
 * @author ZHENG Robert (robert@hase-zheng.net)
 * @copyright Copyright (c) 2026 ZHENG Robert
 * @license Apache-2.0
 */

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// mailDirMode is the mode of the outbox: mail files are plaintext, only the scheduler user may access them.
const mailDirMode fs.FileMode = 0o700

const (
	mailFileSuffix = ".SendMail"
	tempFileSuffix = ".tmp"
)

var lineBreaks = regexp.MustCompile(`[\r\n]+`)

// ensureMailDir makes sure the outbox exists as a directory with mode 0700 and is writable.
// A missing directory (including missing parents) is created; another mode is corrected to 0700
// (if that fails, e.g. for a directory of another owner, only a warning is returned, like
// mitm_maintenance_sendmail does). The returned notes are meant for the audit log.
func ensureMailDir(dir string) ([]string, error) {
	where := describePath(dir)
	var notes []string

	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, mailDirMode); err != nil {
			return nil, fmt.Errorf("mail_dir %s does not exist and cannot be created: %w", where, err)
		}
		// MkdirAll applies the umask; set the mode explicitly.
		if err := os.Chmod(dir, mailDirMode); err != nil {
			return nil, fmt.Errorf("mail_dir %s was created, but its mode cannot be set to 0700: %w", where, err)
		}
		notes = append(notes, fmt.Sprintf("Created mail_dir %s with mode 0700", where))
	case err != nil:
		return nil, fmt.Errorf("mail_dir %s is not accessible: %w", where, err)
	case !info.IsDir():
		return nil, fmt.Errorf("mail_dir %s is not a directory", where)
	case info.Mode().Perm() != mailDirMode:
		if err := os.Chmod(dir, mailDirMode); err != nil {
			notes = append(notes, fmt.Sprintf("WARNING: mail_dir %s has mode %04o instead of 0700 and its mode cannot be changed: %v",
				where, info.Mode().Perm(), err))
		} else {
			notes = append(notes, fmt.Sprintf("WARNING: mail_dir %s had mode %04o; mode set to 0700", where, info.Mode().Perm()))
		}
	}

	// Write check with a *.tmp file, which mitm_maintenance_sendmail ignores.
	probe, err := os.CreateTemp(dir, ".write-check-*"+tempFileSuffix)
	if err != nil {
		return notes, fmt.Errorf("mail_dir %s is not writable: %w", where, err)
	}
	probe.Close()
	os.Remove(probe.Name())
	return notes, nil
}

// describePath quotes a path; a relative path is followed by its absolute path and the working
// directory (the scheduler starts jobs in the directory of its own binary).
func describePath(path string) string {
	if filepath.IsAbs(path) {
		return fmt.Sprintf("%q", path)
	}
	abs, absErr := filepath.Abs(path)
	cwd, cwdErr := os.Getwd()
	if absErr != nil || cwdErr != nil {
		return fmt.Sprintf("%q", path)
	}
	return fmt.Sprintf("%q (%s, working directory %s)", path, abs, cwd)
}

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
