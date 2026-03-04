package notification

import (
	"context"
	"fmt"
	"net/smtp"
	"strconv"
)

// EmailConfig holds the SMTP configuration for sending email notifications.
type EmailConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
}

// SendEmail sends an email notification using the provided SMTP configuration.
func SendEmail(_ context.Context, cfg EmailConfig, to, subject, body string) error {
	if cfg.Host == "" {
		return fmt.Errorf("email host is required")
	}
	if cfg.From == "" {
		return fmt.Errorf("email from address is required")
	}
	if to == "" {
		return fmt.Errorf("email to address is required")
	}

	addr := cfg.Host + ":" + strconv.Itoa(cfg.Port)

	msg := "From: " + cfg.From + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\r\n" +
		"\r\n" +
		body + "\r\n"

	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	}

	if err := smtp.SendMail(addr, auth, cfg.From, []string{to}, []byte(msg)); err != nil {
		return fmt.Errorf("failed to send email: %w", err)
	}

	return nil
}
