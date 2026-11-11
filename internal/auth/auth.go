// Package auth handles passwords and login sessions. Only a SHA-256 of a
// session token is stored, so a leaked database cannot be replayed as logins.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// SessionTTL is how long a login lasts.
const SessionTTL = 7 * 24 * time.Hour

// ErrInvalidCredentials hides whether the email or the password was wrong.
var ErrInvalidCredentials = errors.New("auth: invalid email or password")

// ErrNoSession means the token is unknown or expired.
var ErrNoSession = errors.New("auth: no valid session")

// User is the signed-in person.
type User struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	IsOrganizer bool   `json:"is_organizer"`
}

// dummyHash is compared against when the email is unknown, so a missing
// account takes as long to reject as a wrong password.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password"), bcrypt.DefaultCost)

// HashPassword returns a bcrypt hash for storage.
func HashPassword(password string, cost int) (string, error) {
	if cost == 0 {
		cost = bcrypt.DefaultCost
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		return "", fmt.Errorf("auth: hash: %w", err)
	}
	return string(h), nil
}

// NormalizeEmail trims and lowercases an email address.
func NormalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// Login checks the password and opens a session, returning its raw token.
func Login(ctx context.Context, pool *pgxpool.Pool, email, password string) (string, User, error) {
	var u User
	var hash string
	err := pool.QueryRow(ctx,
		`select id::text, email, display_name, is_organizer, password_hash from users where email = $1`,
		NormalizeEmail(email)).Scan(&u.ID, &u.Email, &u.DisplayName, &u.IsOrganizer, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return "", User{}, ErrInvalidCredentials
	}
	if err != nil {
		return "", User{}, fmt.Errorf("auth: load user: %w", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return "", User{}, ErrInvalidCredentials
	}
	token, err := CreateSession(ctx, pool, u.ID)
	if err != nil {
		return "", User{}, err
	}
	return token, u, nil
}

// CreateSession stores a new session for userID and returns its raw token.
func CreateSession(ctx context.Context, pool *pgxpool.Pool, userID string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if _, err := pool.Exec(ctx,
		`insert into sessions (token_hash, user_id, expires_at) values ($1, $2, now() + $3::interval)`,
		hashToken(token), userID, fmt.Sprintf("%d seconds", int(SessionTTL.Seconds()))); err != nil {
		return "", fmt.Errorf("auth: store session: %w", err)
	}
	return token, nil
}

// UserForToken returns the user behind a live session token.
func UserForToken(ctx context.Context, pool *pgxpool.Pool, token string) (User, error) {
	if token == "" || len(token) > 128 {
		return User{}, ErrNoSession
	}
	var u User
	err := pool.QueryRow(ctx, `
		select u.id::text, u.email, u.display_name, u.is_organizer
		from sessions s join users u on u.id = s.user_id
		where s.token_hash = $1 and s.expires_at > now()`, hashToken(token)).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.IsOrganizer)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNoSession
	}
	if err != nil {
		return User{}, fmt.Errorf("auth: load session: %w", err)
	}
	return u, nil
}

// Logout deletes the session for token.
func Logout(ctx context.Context, pool *pgxpool.Pool, token string) error {
	if _, err := pool.Exec(ctx, `delete from sessions where token_hash = $1`, hashToken(token)); err != nil {
		return fmt.Errorf("auth: logout: %w", err)
	}
	return nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
