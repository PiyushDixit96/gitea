// Copyright 2024 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package actions

import (
	"fmt"
	"strings"

	actions_model "code.gitea.io/gitea/models/actions"
	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/models/perm"
	access_model "code.gitea.io/gitea/models/perm/access"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unit"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/actions"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/reqctx"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/services/context"
	"code.gitea.io/gitea/services/convert"
	notify_service "code.gitea.io/gitea/services/notify"

	"github.com/nektos/act/pkg/jobparser"
	"github.com/nektos/act/pkg/model"
)

func EnableOrDisableWorkflow(ctx *context.APIContext, workflowID string, isEnable bool) error {
	workflow, err := convert.GetActionWorkflow(ctx, ctx.Repo.GitRepo, ctx.Repo.Repository, workflowID)
	if err != nil {
		return err
	}

	cfgUnit := ctx.Repo.Repository.MustGetUnit(ctx, unit.TypeActions)
	cfg := cfgUnit.ActionsConfig()

	if isEnable {
		cfg.EnableWorkflow(workflow.ID)
	} else {
		cfg.DisableWorkflow(workflow.ID)
	}

	return repo_model.UpdateRepoUnit(ctx, cfgUnit)
}

func DispatchActionWorkflow(ctx reqctx.RequestContext, doer *user_model.User, repo *repo_model.Repository, gitRepo *git.Repository, workflowID, ref string, processInputs func(model *model.WorkflowDispatch, inputs map[string]any) error) error {
	if workflowID == "" {
		return util.ErrorWrapLocale(
			util.NewNotExistErrorf("workflowID is empty"),
			"actions.workflow.not_found", workflowID,
		)
	}

	if ref == "" {
		return util.ErrorWrapLocale(
			util.NewNotExistErrorf("ref is empty"),
			"form.target_ref_not_exist", ref,
		)
	}

	// can not rerun job when workflow is disabled
	cfgUnit := repo.MustGetUnit(ctx, unit.TypeActions)
	cfg := cfgUnit.ActionsConfig()
	if cfg.IsWorkflowDisabled(workflowID) {
		return util.ErrorWrapLocale(
			util.NewPermissionDeniedErrorf("workflow is disabled"),
			"actions.workflow.disabled",
		)
	}

	// get target commit of run from specified ref
	refName := git.RefName(ref)
	var runTargetCommit *git.Commit
	var err error
	if refName.IsTag() {
		runTargetCommit, err = gitRepo.GetTagCommit(refName.TagName())
	} else if refName.IsBranch() {
		runTargetCommit, err = gitRepo.GetBranchCommit(refName.BranchName())
	} else {
		refName = git.RefNameFromBranch(ref)
		runTargetCommit, err = gitRepo.GetBranchCommit(ref)
	}
	if err != nil {
		return util.ErrorWrapLocale(
			util.NewNotExistErrorf("ref %q doesn't exist", ref),
			"form.target_ref_not_exist", ref,
		)
	}

	// get workflow entry from runTargetCommit
	_, entries, err := actions.ListWorkflows(runTargetCommit)
	if err != nil {
		return err
	}

	// find workflow from commit
	var workflows []*jobparser.SingleWorkflow
	var entry *git.TreeEntry

	run := &actions_model.ActionRun{
		Title:             strings.SplitN(runTargetCommit.CommitMessage, "\n", 2)[0],
		RepoID:            repo.ID,
		Repo:              repo,
		OwnerID:           repo.OwnerID,
		WorkflowID:        workflowID,
		TriggerUserID:     doer.ID,
		TriggerUser:       doer,
		Ref:               string(refName),
		CommitSHA:         runTargetCommit.ID.String(),
		IsForkPullRequest: false,
		Event:             "workflow_dispatch",
		TriggerEvent:      "workflow_dispatch",
		Status:            actions_model.StatusWaiting,
	}

	for _, e := range entries {
		if e.Name() != workflowID {
			continue
		}
		entry = e
		break
	}

	if entry == nil {
		return util.ErrorWrapLocale(
			util.NewNotExistErrorf("workflow %q doesn't exist", workflowID),
			"actions.workflow.not_found", workflowID,
		)
	}

	content, err := actions.GetContentFromEntry(entry)
	if err != nil {
		return err
	}

	giteaCtx := GenerateGiteaContext(run, nil)

	workflows, err = jobparser.Parse(content, jobparser.WithGitContext(giteaCtx.ToGitHubContext()))
	if err != nil {
		return err
	}

	if len(workflows) > 0 && workflows[0].RunName != "" {
		run.Title = workflows[0].RunName
	}

	if len(workflows) == 0 {
		return util.ErrorWrapLocale(
			util.NewNotExistErrorf("workflow %q doesn't exist", workflowID),
			"actions.workflow.not_found", workflowID,
		)
	}

	// get inputs from post
	workflow := &model.Workflow{
		RawOn: workflows[0].RawOn,
	}
	inputsWithDefaults := make(map[string]any)
	if workflowDispatch := workflow.WorkflowDispatchConfig(); workflowDispatch != nil {
		if err = processInputs(workflowDispatch, inputsWithDefaults); err != nil {
			return err
		}
	}

	// ctx.Req.PostForm -> WorkflowDispatchPayload.Inputs -> ActionRun.EventPayload -> runner: ghc.Event
	// https://docs.github.com/en/actions/learn-github-actions/contexts#github-context
	// https://docs.github.com/en/webhooks/webhook-events-and-payloads#workflow_dispatch
	workflowDispatchPayload := &api.WorkflowDispatchPayload{
		Workflow:   workflowID,
		Ref:        ref,
		Repository: convert.ToRepo(ctx, repo, access_model.Permission{AccessMode: perm.AccessModeNone}),
		Inputs:     inputsWithDefaults,
		Sender:     convert.ToUserWithAccessMode(ctx, doer, perm.AccessModeNone),
	}

	var eventPayload []byte
	if eventPayload, err = workflowDispatchPayload.JSONPayload(); err != nil {
		return fmt.Errorf("JSONPayload: %w", err)
	}
	run.EventPayload = string(eventPayload)

	// cancel running jobs of the same workflow
	if err := CancelPreviousJobs(
		ctx,
		run.RepoID,
		run.Ref,
		run.WorkflowID,
		run.Event,
	); err != nil {
		log.Error("CancelRunningJobs: %v", err)
	}

	// Insert the action run and its associated jobs into the database
	if err := actions_model.InsertRun(ctx, run, workflows); err != nil {
		return fmt.Errorf("InsertRun: %w", err)
	}

	allJobs, err := db.Find[actions_model.ActionRunJob](ctx, actions_model.FindRunJobOptions{RunID: run.ID})
	if err != nil {
		log.Error("FindRunJobs: %v", err)
	}
	CreateCommitStatus(ctx, allJobs...)
	if len(allJobs) > 0 {
		job := allJobs[0]
		err := job.LoadRun(ctx)
		if err != nil {
			log.Error("LoadRun: %v", err)
		} else {
			notify_service.WorkflowRunStatusUpdate(ctx, job.Run.Repo, job.Run.TriggerUser, job.Run)
		}
	}
	for _, job := range allJobs {
		notify_service.WorkflowJobStatusUpdate(ctx, repo, doer, job, nil)
	}
	return nil
}
