package deploy

import (
	"context"
	"fmt"
	"strconv"

	"github.com/dymmer-code/dymploy/internal/manifest"
)

// finishDeployment keeps the original failure even when recovery succeeds. Separate bounded
// contexts allow recovery after a timeout and finalization after cancellation. An explicit abort
// skips recovery: it may have revoked this job's authorization to change the application.
func (d *Deployer) finishDeployment(parent context.Context, conn Conn, r Request, env map[string]string, phase string, result *int, runErr *error) {
	failed := *result != 0 || *runErr != nil
	status := "succeeded"
	if failed {
		status = "failed"
	}
	if parent.Err() != nil {
		status = "aborted"
	}
	env = merge(nil, env)
	env["DYMPLOY_DEPLOY_STATUS"] = status
	env["DYMPLOY_FAILED_PHASE"] = ""
	if failed {
		env["DYMPLOY_FAILED_PHASE"] = phase
	}
	env["DYMPLOY_EXIT_CODE"] = strconv.Itoa(*result)
	env["DYMPLOY_UNDO_STATUS"] = "not_run"
	if failed && (phase == "steps" || phase == "check") {
		switch {
		case len(r.Deploy.StepsUndo) == 0:
			// Preserve the existing no-rollback behavior and its diagnostic.
			if *runErr == nil && (*result == ExitStep || *result == ExitCheck) {
				d.noUndo(r)
			}
		case parent.Err() != nil:
			env["DYMPLOY_UNDO_STATUS"] = "skipped"
			r.Log.System("the job was stopped: deploy.steps_undo was not run; maintenance must remain in place")
		default:
			env["DYMPLOY_UNDO_STATUS"] = "failed"
			if runLifecycleSteps(parent, conn, r, "steps_undo", r.Deploy.StepsUndo, env, false) {
				env["DYMPLOY_UNDO_STATUS"] = "succeeded"
			}
		}
	}
	if parent.Err() != nil {
		env["DYMPLOY_DEPLOY_STATUS"] = "aborted"
	}
	if r.Deploy.Finally != nil {
		if !runLifecycleSteps(context.Background(), conn, r, "finally", r.Deploy.Finally.Steps, env, true) && !failed {
			*result = ExitFinally
		}
	}
}

// Recovery stops at the first failure so later steps cannot unlock an unhealthy target.
// Finalization attempts independent cleanup/notification steps even if an earlier one fails.
func runLifecycleSteps(parent context.Context, conn Conn, r Request, phase string, steps []manifest.DeployStep, env map[string]string, keepGoing bool) bool {
	ctx, cancel := context.WithTimeout(parent, undoTimeout)
	defer cancel()
	ok := true
	for i, s := range steps {
		name := stepName(s.Name, i)
		r.Log.System(fmt.Sprintf("%s [%d/%d] %s", phase, i+1, len(steps), name))
		code, err := conn.Run(ctx, s.Run, merge(s.Env, env), r.Log.Stdout(), r.Log.Stderr())
		if code != 0 || err != nil {
			ok = false
			r.Log.System(fmt.Sprintf("%s %s failed (exit code %d, error %v)", phase, name, code, err))
			if !keepGoing || ctx.Err() != nil {
				return false
			}
		}
	}
	return ok
}
