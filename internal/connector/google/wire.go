package google

import (
	"context"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WireMailSender builds the send adapter every binary should use (criterion 13).
//
// One function rather than three copies of the same if/else: the dashboard,
// opsctl and ops-mcp all need identical routing, and three hand-written copies
// is how one of them silently keeps sending through the wrong transport after a
// change. Returns nil when nothing can be wired, so the caller can log it and
// carry on — send_delivery already refuses cleanly with "no gmail send adapter
// wired" rather than sending by some other route.
//
// Routing is per-account and read from the row (auth_type), not from process
// configuration: an app-password mailbox goes over SMTP, an OAuth mailbox keeps
// the existing Gmail-API path byte for byte, and both coexist during a
// migration. The OAuth branch preserves today's precedence exactly — bridge
// first so OAuth tokens have one owner, then the database-token adapter.
func WireMailSender(pool *pgxpool.Pool) (*MailSender, string) {
	key := os.Getenv("OPS_TOKEN_KEY")

	var oauth interface {
		Send(ctx context.Context, fromUserID string, rawMIME []byte, threadID string) (string, error)
	}
	how := ""

	if binary := os.Getenv("GMAIL_CONNECTOR_BRIDGE"); binary != "" {
		if bridge, err := NewCommandBridge(binary); err == nil {
			oauth = &BridgeSender{Bridge: bridge}
			how = "bridge"
		}
	}
	if oauth == nil && key != "" {
		secretFile := os.Getenv("GOOGLE_CLIENT_SECRET_FILE")
		if secretFile == "" {
			home, _ := os.UserHomeDir()
			secretFile = filepath.Join(home, ".config", "switchboard", "google_client_secret.json")
		}
		if oauthCfg, err := LoadOAuthConfig(secretFile, ""); err == nil {
			oauth = &AccountSender{Pool: pool, OAuthCfg: oauthCfg, TokenKey: key}
			how = "oauth"
		}
	}

	var smtp *SMTPSender
	if key != "" {
		smtp = &SMTPSender{Pool: pool, TokenKey: key}
		if how == "" {
			how = "smtp"
		} else {
			how += "+smtp"
		}
	}

	if oauth == nil && smtp == nil {
		return nil, ""
	}
	return &MailSender{Pool: pool, TokenKey: key, SMTP: smtp, OAuth: oauth}, how
}
