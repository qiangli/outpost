package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/postinstall"
	"github.com/spf13/cobra"
)

const runtimeOS = runtime.GOOS

func postInstallIdentity() (string, string) {
	home, _ := os.UserHomeDir()
	if u, err := user.Current(); err == nil {
		return u.Username, home
	}
	return os.Getenv("USER"), home
}
func postInstallRead(ctx context.Context) postinstall.Report {
	u, h := postInstallIdentity()
	return postinstall.Read(ctx, runtimeOS, u, h, postinstall.Exec)
}
func verifyCmd() *cobra.Command {
	var post, reboot bool
	cmd := &cobra.Command{Use: "verify --post-install [--reboot]", Short: "Verify boot service and sleep policy; save raw PASS/FAIL evidence", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !post {
				return fmt.Errorf("specify --post-install (and optionally --reboot)")
			}
			dir, err := conf.ResolveCacheDir()
			if err != nil {
				return err
			}
			r := postInstallRead(cmd.Context())
			if err = postinstall.Write(dir, postinstall.ReportName, r); err != nil {
				return err
			}
			if reboot {
				if err = postinstall.Schedule(cmd.Context(), dir, r, postinstall.Exec); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Reboot scheduled in one minute; reboot result remains FAIL until boot and server checks pass. Evidence: %s\n", filepath.Join(dir, postinstall.RebootReportName))
				return nil
			}
			if err = json.NewEncoder(cmd.OutOrStdout()).Encode(r); err != nil {
				return err
			}
			if r.Result != "PASS" {
				return fmt.Errorf("post-install verification failed; evidence: %s", filepath.Join(dir, postinstall.ReportName))
			}
			return nil
		}}
	cmd.Flags().BoolVar(&post, "post-install", false, "Read back sleep policy and boot service registration")
	cmd.Flags().BoolVar(&reboot, "reboot", false, "Schedule a reboot in one minute and record unattended server reconnection on next startup")
	return cmd
}
func serviceDoctor() []doctorCheck {
	r := postInstallRead(context.Background())
	sleepStatus, serviceStatus := "warn", "warn"
	if r.Sleep == "disabled" {
		sleepStatus = "ok"
	}
	if r.Service == "boot" {
		serviceStatus = "ok"
	}
	return []doctorCheck{{"sleep:", sleepStatus, r.Sleep}, {"service:", serviceStatus, r.Service}}
}
