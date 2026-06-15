package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/streamingo/dstream/internal/auth"
	"github.com/streamingo/dstream/internal/config"
	"github.com/streamingo/dstream/internal/store"
)

func adminCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "admin",
		Short: "Admin operations (promote super-admin, bootstrap orgs)",
	}
	c.AddCommand(promoteCmd(), bootstrapCmd())
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

func bootstrapCmd() *cobra.Command {
	var email, orgSlug, projectSlug, keyName string
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Create user (if missing) + org + project + API key in one shot",
		RunE: func(_ *cobra.Command, _ []string) error {
			if email == "" || orgSlug == "" || projectSlug == "" {
				return errors.New("--email, --org, --project all required")
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
				Role:   "owner",
			}); err != nil {
				return fmt.Errorf("add member: %w", err)
			}

			project, err := q.GetProjectByOrgAndSlug(ctx, store.GetProjectByOrgAndSlugParams{
				OrgID: org.ID,
				Slug:  projectSlug,
			})
			if err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("lookup project: %w", err)
				}
				project, err = q.CreateProject(ctx, store.CreateProjectParams{
					OrgID: org.ID,
					Name:  projectSlug,
					Slug:  projectSlug,
				})
				if err != nil {
					return fmt.Errorf("create project: %w", err)
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
				ProjectID: project.ID,
				Name:      label,
				Prefix:    prefix,
				KeyHash:   hash,
			}); err != nil {
				return fmt.Errorf("create api key: %w", err)
			}

			fmt.Printf("user:    %s\n", email)
			fmt.Printf("org:     %s (id=%s)\n", orgSlug, store.GoUUID(org.ID))
			fmt.Printf("project: %s (id=%s)\n", projectSlug, store.GoUUID(project.ID))
			fmt.Printf("api key: %s\n", full)
			fmt.Println("\nSet it in your shell:")
			fmt.Printf("  export DSTREAM_API_KEY=%s\n", full)
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "User email (created if missing)")
	cmd.Flags().StringVar(&orgSlug, "org", "", "Org slug (created if missing)")
	cmd.Flags().StringVar(&projectSlug, "project", "", "Project slug (created if missing)")
	cmd.Flags().StringVar(&keyName, "key-name", "bootstrap", "Label for the new API key")
	return cmd
}
