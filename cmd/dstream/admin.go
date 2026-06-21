package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/spf13/cobra"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/config"
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
		orgCmd(),
		memberCmd(),
		keyCmd(),
	)
	return c
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

			org, err := q.GetOrganizationBySlug(ctx, orgSlug)
			if err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("lookup org: %w", err)
				}
				org, err = q.CreateOrganization(ctx, store.CreateOrganizationParams{
					Name: orgSlug,
					Slug: orgSlug,
				})
				if err != nil {
					return fmt.Errorf("create org: %w", err)
				}
			}

			if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{
				OrgID:  org.ID,
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

			full, prefix, hash, err := auth.NewAPIKey()
			if err != nil {
				return fmt.Errorf("gen api key: %w", err)
			}
			label := keyName
			if label == "" {
				label = "bootstrap"
			}
			if _, err := q.CreateAPIKey(ctx, store.CreateAPIKeyParams{
				OrgID:   org.ID,
				Name:    label,
				Prefix:  prefix,
				KeyHash: hash,
			}); err != nil {
				return fmt.Errorf("create api key: %w", err)
			}

			fmt.Printf("user:    %s\n", email)
			fmt.Printf("org:     %s (id=%s)\n", orgSlug, store.GoUUID(org.ID))
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
