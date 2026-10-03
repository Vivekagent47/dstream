package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/spf13/cobra"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/config"
	"github.com/Vivekagent47/dstream/internal/opevents"
	"github.com/Vivekagent47/dstream/internal/store"
)

func adminCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "admin",
		Short: "Admin operations (promote super-admin, bootstrap orgs, manage orgs/members/keys)",
	}
	c.AddCommand(
		promoteCmd(),
		bootstrapCmd(),
		magicLinkCmd(),
		orgCmd(),
		memberCmd(),
		keyCmd(),
	)
	return c
}

// magicLinkCmd mints a sign-in link and prints it, bypassing
// DSTREAM_OIDC_ENFORCE.
//
// This exists because enforcement without a break-glass is a foot-gun: an
// expired client secret, rotated signing keys, or a down discovery endpoint
// locks every human out of the deployment — including whoever would fix it.
// Reaching this command requires shell access to the host, which is a stronger
// factor than any IdP, and it writes an audit row so the use is visible.
//
// It works because enforcement gates minting, not redemption:
// POST /api/auth/magic-link/verify stays open, so the token printed here is
// redeemable even with DSTREAM_OIDC_ENFORCE=true.
func magicLinkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "magic-link <email>",
		Short: "Mint a sign-in link for an existing user (break-glass; bypasses SSO enforcement)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			email := strings.ToLower(strings.TrimSpace(args[0]))
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx := context.Background()
			pool, err := store.NewPool(ctx, cfg.DB.URL, cfg.DB.MaxConns)
			if err != nil {
				return err
			}
			defer pool.Close()
			q := store.New(pool)

			// Existing users only. Minting for an unknown address would let a
			// typo create an account through the break-glass, and the audit row
			// below needs a real user to hang off.
			u, err := q.GetUserByEmail(ctx, email)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("user %s does not exist; have them sign in once, or use `dstream admin bootstrap`", email)
				}
				return fmt.Errorf("lookup user: %w", err)
			}
			// audit_logs.org_id is the tenant scope the trail is read by, so
			// a row with no org is a row nobody can see. Resolved before
			// minting so a user with no org fails without a live token.
			orgs, err := q.ListOrgsForUser(ctx, u.ID)
			if err != nil {
				return fmt.Errorf("list orgs for %s: %w", email, err)
			}
			if len(orgs) == 0 {
				return fmt.Errorf("user %s belongs to no org; cannot file an audit row for a break-glass sign-in", email)
			}

			token, err := auth.IssueMagicLink(ctx, q, email, cfg.MagicLinkTTL)
			if err != nil {
				return fmt.Errorf("issue magic link: %w", err)
			}

			// NOT audit.Log: it resolves the actor from a Principal in ctx and
			// is a documented no-op with a warning when there is none
			// (internal/audit/log.go) — "out-of-band privileged actions should
			// not flow through here". A CLI invocation has no principal, so
			// audit.Log would silently record nothing, which is the opposite of
			// what a break-glass needs. Insert the row directly instead.
			//
			// host + os_user are the operator attribution the actor columns
			// cannot carry (see below): metadata.actor="cli" says a shell did
			// it, these say which one. "unknown" rather than "" — an empty
			// string in an audit row reads as a missing field.
			host, herr := os.Hostname()
			if herr != nil {
				fmt.Fprintf(os.Stderr, "warn: hostname for audit row: %v\n", herr)
				host = "unknown"
			}
			osUser := os.Getenv("USER")
			if osUser == "" {
				osUser = "unknown"
			}
			meta, err := json.Marshal(map[string]any{
				"email":   email,
				"reason":  "sso_enforced_break_glass",
				"actor":   "cli",
				"host":    host,
				"os_user": osUser,
			})
			if err != nil {
				return fmt.Errorf("encode audit metadata: %w", err)
			}
			if err := q.InsertAuditLog(ctx, store.InsertAuditLogParams{
				OrgID: orgs[0].ID,
				// audit_logs.org_id is ON DELETE SET NULL, so without the
				// snapshot a break-glass row outlives its org with no org
				// identity at all. audit.Log sets it for the same reason.
				OrgNameSnapshot: &orgs[0].Name,
				// A NULL actor is NOT insertable: audit_logs_check requires
				// exactly one of (actor_user_id, actor_api_key_id) to be
				// non-null, and the real actor here is whoever holds shell
				// access, not a dstream user. So the row names its target as
				// its own actor and metadata.actor="cli" carries the real
				// provenance — the alternative, skipping the row, would make
				// the break-glass invisible, which is worse than a
				// self-referential one.
				ActorUserID:        u.ID,
				ActorEmailSnapshot: &email,
				Action:             "auth.break_glass_magic_link",
				TargetType:         "user",
				TargetID:           u.ID,
				Metadata:           meta,
			}); err != nil {
				// The token exists but was never printed and is never logged,
				// so it is unusable by anyone and expires on its own. Fail
				// loudly rather than hand out an unaudited sign-in link.
				return fmt.Errorf("record break-glass audit row: %w", err)
			}

			fmt.Printf("%s/auth/verify?token=%s\n", strings.TrimRight(cfg.AppBaseURL, "/"), url.QueryEscape(token))
			fmt.Println("\nThis link bypasses SSO enforcement and is single-use. It expires in", cfg.MagicLinkTTL)
			return nil
		},
	}
}

func promoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "promote <email>",
		Short: "Promote a user to super-admin",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx := context.Background()
			pool, err := store.NewPool(ctx, cfg.DB.URL, cfg.DB.MaxConns)
			if err != nil {
				return err
			}
			defer pool.Close()
			q := store.New(pool)
			if err := q.PromoteUserToSuperAdmin(ctx, args[0]); err != nil {
				return fmt.Errorf("promote: %w", err)
			}
			fmt.Printf("promoted %s to super-admin\n", args[0])
			return nil
		},
	}
}

// bootstrapCmd creates (or reuses) a user + org and mints an org-scoped API
// key in one shot. Kept alongside the finer-grained subcommands as a
// convenience for first-run setup.
func bootstrapCmd() *cobra.Command {
	var email, orgSlug, keyName string
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Create user (if missing) + org + API key in one shot",
		RunE: func(_ *cobra.Command, _ []string) error {
			if email == "" || orgSlug == "" {
				return errors.New("--email and --org are required")
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx := context.Background()
			pool, err := store.NewPool(ctx, cfg.DB.URL, cfg.DB.MaxConns)
			if err != nil {
				return err
			}
			defer pool.Close()
			q := store.New(pool)

			email = strings.ToLower(strings.TrimSpace(email))
			user, err := q.GetUserByEmail(ctx, email)
			if err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("lookup user: %w", err)
				}
				user, err = q.CreateUser(ctx, store.CreateUserParams{Email: email})
				if err != nil {
					return fmt.Errorf("create user: %w", err)
				}
			}

			// GetOrganizationBySlug and CreateOrganization return distinct
			// pinned row types (see db/queries/identity.sql), so only the
			// shared field this function needs — ID — is carried across the
			// two branches.
			var orgID pgtype.UUID
			if orgRow, err := q.GetOrganizationBySlug(ctx, orgSlug); err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("lookup org: %w", err)
				}
				created, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{
					Name: orgSlug,
					Slug: orgSlug,
				})
				if err != nil {
					return fmt.Errorf("create org: %w", err)
				}
				orgID = created.ID
			} else {
				orgID = orgRow.ID
			}

			if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{
				OrgID:  orgID,
				UserID: user.ID,
				Role:   string(auth.RoleOwner),
			}); err != nil {
				// Re-running bootstrap must be idempotent — tolerate the
				// PK collision on (org_id, user_id) that says "already a
				// member". Any other DB error still fails the command.
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
					return fmt.Errorf("add member: %w", err)
				}
			}

			// Idempotent; backfill covers pre-existing orgs. Admin tooling
			// tolerates a seed failure (log + continue).
			if _, err := opevents.SeedOperationalApp(ctx, q, store.GoUUID(orgID)); err != nil {
				fmt.Fprintf(os.Stderr, "warn: seed operational app: %v\n", err)
			}

			full, prefix, hash, err := auth.NewAPIKey()
			if err != nil {
				return fmt.Errorf("gen api key: %w", err)
			}
			label := keyName
			if label == "" {
				label = "bootstrap"
			}
			if _, err := q.CreateAPIKey(ctx, store.CreateAPIKeyParams{
				OrgID:   orgID,
				Name:    label,
				Prefix:  prefix,
				KeyHash: hash,
				Role:    string(auth.RoleAdmin), // bootstrap key drives setup
			}); err != nil {
				return fmt.Errorf("create api key: %w", err)
			}

			fmt.Printf("user:    %s\n", email)
			fmt.Printf("org:     %s (id=%s)\n", orgSlug, store.GoUUID(orgID))
			fmt.Printf("api key: %s\n", full)
			fmt.Println("\nSet it in your shell:")
			fmt.Printf("  export DSTREAM_API_KEY=%s\n", full)
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "User email (created if missing)")
	cmd.Flags().StringVar(&orgSlug, "org", "", "Org slug (created if missing)")
	cmd.Flags().StringVar(&keyName, "key-name", "bootstrap", "Label for the new API key")
	return cmd
}

// orgCmd is the container for `dstream admin org *` subcommands.
func orgCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "org",
		Short: "Org operations",
	}
	c.AddCommand(orgCreateCmd())
	return c
}

// orgCreateCmd implements `dstream admin org create <name> <owner_email>`.
// Errors if the user does not exist — use `bootstrap` for that case.
func orgCreateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "create <name> <owner_email>",
		Short: "Create an org and assign an existing user as owner",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			email := strings.ToLower(strings.TrimSpace(args[1]))
			if name == "" {
				return errors.New("name required")
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx := context.Background()
			pool, err := store.NewPool(ctx, cfg.DB.URL, cfg.DB.MaxConns)
			if err != nil {
				return err
			}
			defer pool.Close()
			q := store.New(pool)

			user, err := q.GetUserByEmail(ctx, email)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("user %s does not exist; use `dstream admin bootstrap` or have them sign in first", email)
				}
				return fmt.Errorf("lookup user: %w", err)
			}
			slug := slugify(name)
			org, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{
				Name: name,
				Slug: slug,
			})
			if err != nil {
				return fmt.Errorf("create org: %w", err)
			}
			if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{
				OrgID:  org.ID,
				UserID: user.ID,
				Role:   string(auth.RoleOwner),
			}); err != nil {
				return fmt.Errorf("add owner: %w", err)
			}
			// Idempotent; admin tooling tolerates a seed failure (log + continue).
			if _, err := opevents.SeedOperationalApp(ctx, q, store.GoUUID(org.ID)); err != nil {
				fmt.Fprintf(os.Stderr, "warn: seed operational app: %v\n", err)
			}
			fmt.Printf("org:   %s (id=%s, slug=%s)\n", name, store.GoUUID(org.ID), slug)
			fmt.Printf("owner: %s (id=%s)\n", email, store.GoUUID(user.ID))
			return nil
		},
	}
}

// memberCmd is the container for `dstream admin member *` subcommands.
func memberCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "member",
		Short: "Org membership operations",
	}
	c.AddCommand(memberAddCmd())
	return c
}

// memberAddCmd implements `dstream admin member add <org_id> <email> <role>`.
// Both the user and the org must already exist.
func memberAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <org_id> <email> <role>",
		Short: "Add an existing user to an org with the given role (owner|admin|member)",
		Args:  cobra.ExactArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			orgID, err := uuid.Parse(strings.TrimSpace(args[0]))
			if err != nil {
				return fmt.Errorf("invalid org_id: %w", err)
			}
			email := strings.ToLower(strings.TrimSpace(args[1]))
			role := strings.ToLower(strings.TrimSpace(args[2]))
			switch auth.Role(role) {
			case auth.RoleOwner, auth.RoleAdmin, auth.RoleMember:
			default:
				return fmt.Errorf("role must be owner, admin, or member (got %q)", role)
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx := context.Background()
			pool, err := store.NewPool(ctx, cfg.DB.URL, cfg.DB.MaxConns)
			if err != nil {
				return err
			}
			defer pool.Close()
			q := store.New(pool)

			if _, err := q.GetOrganizationByID(ctx, store.UUID(orgID)); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("org %s not found", orgID)
				}
				return fmt.Errorf("lookup org: %w", err)
			}
			user, err := q.GetUserByEmail(ctx, email)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("user %s does not exist; ask them to sign in first to create their account", email)
				}
				return fmt.Errorf("lookup user: %w", err)
			}
			if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{
				OrgID:  store.UUID(orgID),
				UserID: user.ID,
				Role:   role,
			}); err != nil {
				return fmt.Errorf("add member: %w", err)
			}
			fmt.Printf("added %s to org %s as %s\n", email, orgID, role)
			return nil
		},
	}
}

// keyCmd is the container for `dstream admin key *` subcommands.
func keyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "key",
		Short: "API key operations",
	}
	c.AddCommand(keyCreateCmd())
	return c
}

// keyCreateCmd implements `dstream admin key create <org_id> <name>`. Prints
// the full secret ONCE — it cannot be retrieved later.
func keyCreateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "create <org_id> <name>",
		Short: "Mint an org-scoped API key (prints the secret once)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			orgID, err := uuid.Parse(strings.TrimSpace(args[0]))
			if err != nil {
				return fmt.Errorf("invalid org_id: %w", err)
			}
			name := strings.TrimSpace(args[1])
			if name == "" {
				return errors.New("name required")
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx := context.Background()
			pool, err := store.NewPool(ctx, cfg.DB.URL, cfg.DB.MaxConns)
			if err != nil {
				return err
			}
			defer pool.Close()
			q := store.New(pool)

			if _, err := q.GetOrganizationByID(ctx, store.UUID(orgID)); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("org %s not found", orgID)
				}
				return fmt.Errorf("lookup org: %w", err)
			}
			full, prefix, hash, err := auth.NewAPIKey()
			if err != nil {
				return fmt.Errorf("gen key: %w", err)
			}
			row, err := q.CreateAPIKey(ctx, store.CreateAPIKeyParams{
				OrgID:   store.UUID(orgID),
				Name:    name,
				Prefix:  prefix,
				KeyHash: hash,
				Role:    string(auth.RoleAdmin),
			})
			if err != nil {
				return fmt.Errorf("create key: %w", err)
			}
			fmt.Printf("key id: %s\n", store.GoUUID(row.ID))
			fmt.Printf("name:   %s\n", name)
			fmt.Printf("key:    %s\n", full)
			fmt.Println("\nSave it now — the secret is not retrievable later.")
			fmt.Println("Set it in your shell:")
			fmt.Printf("  export DSTREAM_API_KEY=%s\n", full)
			return nil
		},
	}
}

// slugify derives a URL-safe slug from a free-form name. Lowercases, keeps
// [a-z0-9], collapses anything else into single dashes, trims leading and
// trailing dashes, falls back to "org" for empty input, and appends a 3-byte
// random hex suffix so two orgs with identical names don't collide.
//
// Intentionally duplicated from internal/api/orgs.go's slugifyName to keep
// the cmd binary self-contained; this function runs at most once per CLI
// invocation, so the duplication has no runtime cost and avoids widening
// internal package surface.
func slugify(name string) string {
	b := make([]byte, 0, len(name))
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b = append(b, byte(r))
			prevDash = false
		default:
			if !prevDash && len(b) > 0 {
				b = append(b, '-')
				prevDash = true
			}
		}
	}
	for len(b) > 0 && b[len(b)-1] == '-' {
		b = b[:len(b)-1]
	}
	if len(b) == 0 {
		b = []byte("org")
	}
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	return string(b) + "-" + hex.EncodeToString(suffix[:])
}
