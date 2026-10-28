package cmd

import (
	"fmt"
	"github.com/dymmer-code/dymploy/internal/runner"
	"github.com/spf13/cobra"
)

func newCacheCmd() *cobra.Command {
	root := &cobra.Command{Use: "cache", Short: "Inspect and invalidate build caches"}
	for _, invalidate := range []bool{false, true} {
		var cfg, socket string
		var q runner.CacheRequest
		var asJSON bool
		use, short := "list [provider/owner/repository]", "List cache generations and usage"
		args := cobra.MaximumNArgs(1)
		if invalidate {
			use = "invalidate <provider/owner/repository>"
			short = "Give future builds a new empty cache generation"
			args = cobra.ExactArgs(1)
		}
		c := &cobra.Command{Use: use, Short: short, Args: args, RunE: func(c *cobra.Command, args []string) error {
			if len(args) > 0 {
				q.Repository = args[0]
			}
			cl, err := clientFor(socket, cfg)
			if err != nil {
				return err
			}
			if invalidate {
				out, err := cl.InvalidateCache(c.Context(), q)
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(c.OutOrStdout(), out)
				}
				fmt.Fprintf(c.OutOrStdout(), "invalidated %d caches; freed %d bytes (active generations retained)\n", out.Invalidated, out.Freed)
				return nil
			}
			items, err := cl.Caches(c.Context(), q)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(c.OutOrStdout(), items)
			}
			for _, x := range items {
				gen := x.Generation
				if gen == "" {
					gen = "legacy"
				}
				fmt.Fprintf(c.OutOrStdout(), "%s %s %s generation=%s bytes=%d in_use=%t retired=%t key=%q\n", x.Repository, x.Scope, x.Name, gen, x.Bytes, x.InUse, x.Retired, x.Key)
			}
			return nil
		}}
		socketFlags(c, &cfg, &socket)
		c.Flags().StringVar(&q.Name, "name", "", "select one cache")
		c.Flags().StringVar(&q.Scope, "scope", "", "select repository, @branch-NAME, @tags or @other")
		c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
		if invalidate {
			c.Flags().BoolVar(&q.All, "all", false, "invalidate all matching caches in this repository")
		}
		root.AddCommand(c)
	}
	return root
}
