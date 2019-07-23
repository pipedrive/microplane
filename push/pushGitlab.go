package push

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/xanzy/go-gitlab"
)

// Push pushes the commit to Github and opens a pull request
func GitlabPush(ctx context.Context, input Input, githubLimiter *time.Ticker, pushLimiter *time.Ticker) (Output, error) {
	// Get the commit SHA from the last commit
	cmd := Command{Path: "git", Args: []string{"log", "-1", "--pretty=format:%H"}}
	gitLog := exec.CommandContext(ctx, cmd.Path, cmd.Args...)
	gitLog.Dir = input.PlanDir
	gitLogOutput, err := gitLog.CombinedOutput()
	if err != nil {
		return Output{Success: false}, errors.New(string(gitLogOutput))
	}

	// Push the commit
	gitHeadBranch := fmt.Sprintf("HEAD:%s", input.BranchName)
	cmd = Command{Path: "git", Args: []string{"push", "-f", "origin", gitHeadBranch}}
	gitPush := exec.CommandContext(ctx, cmd.Path, cmd.Args...)
	gitPush.Dir = input.PlanDir
	if output, err := gitPush.CombinedOutput(); err != nil {
		return Output{Success: false}, errors.New(string(output))
	}

	// Create Gitlab Client
	client := gitlab.NewClient(nil, os.Getenv("GITLAB_API_TOKEN"))
	client.SetBaseURL(os.Getenv("GITLAB_URL"))

	// Open a pull request, if one doesn't exist already
	head := input.BranchName
	base := "master"

	// Determine PR title and body
	// Title is first line of commit message.
	// Body is given by body-file if it exists or is the remainder of the commit message after title.
	title := input.CommitMessage
	body := ""
	splitMsg := strings.SplitN(input.CommitMessage, "\n", 2)
	if len(splitMsg) == 2 {
		title = splitMsg[0]
		if input.PRBody == "" {
			body = splitMsg[1]
		}
	}

	pr, err := findOrCreateGitlabMR(ctx, client, input.RepoOwner, input.RepoName, &gitlab.CreateMergeRequestOptions{
		Title:        &title,
		Description:  &body,
		SourceBranch: &head,
		TargetBranch: &base,
	}, githubLimiter, pushLimiter)
	if err != nil {
		return Output{Success: false}, err
	}
	pipelineStatus, err := GetPipelineStatus(client, input.RepoOwner, input.RepoName, &gitlab.ListProjectPipelinesOptions{SHA: &pr.SHA})
	if err != nil {
		return Output{Success: false}, err
	}
	return Output{
		Success:                   true,
		CommitSHA:                 pr.SHA,
		PullRequestNumber:         pr.ID,
		PullRequestURL:            pr.WebURL,
		PullRequestCombinedStatus: pipelineStatus,
		PullRequestAssignee:       input.PRAssignee,
		CircleCIBuildURL:          pr.Pipeline.Ref,
	}, nil
}

func findOrCreateGitlabMR(ctx context.Context, client *gitlab.Client, owner string, name string, pull *gitlab.CreateMergeRequestOptions, githubLimiter *time.Ticker, pushLimiter *time.Ticker) (*gitlab.MergeRequest, error) {
	var pr *gitlab.MergeRequest
	prStatus := "opened"
	<-pushLimiter.C
	<-githubLimiter.C
	pid := fmt.Sprintf("%s/%s", owner, name)
	newMR, _, err := client.MergeRequests.CreateMergeRequest(pid, pull)
	if err != nil && strings.Contains(err.Error(), "merge request already exists") {
		<-githubLimiter.C
		existingMRs, _, err := client.MergeRequests.ListMergeRequests(&gitlab.ListMergeRequestsOptions{
			SourceBranch: pull.SourceBranch,
			TargetBranch: pull.TargetBranch,
			State:        &prStatus,
		})
		if err != nil {
			return nil, err
		} else if len(existingMRs) != 1 {
			return nil, errors.New("unexpected: found more than 1 MR for branch")
		}
		pr = existingMRs[0]
		//If needed, update PR title and body
		if different(&pr.Title, pull.Title) || different(&pr.Description, pull.Description) {
			pr.Title = *pull.Title
			pr.Description = *pull.Description
			<-githubLimiter.C
			pr, _, err = client.MergeRequests.UpdateMergeRequest(pid, existingMRs[0].ID, &gitlab.UpdateMergeRequestOptions{
				TargetBranch: pull.TargetBranch,
			})
			if err != nil {
				return nil, err
			}
		}

	} else if err != nil {
		return nil, err
	} else {
		pr = newMR
	}
	return pr, nil
}

// GetPipelineStatus returns status of pipeline, if pipeline is absent, returns unknown string
func GetPipelineStatus(client *gitlab.Client, owner string, name string, opts *gitlab.ListProjectPipelinesOptions) (string, error) {
	pid := fmt.Sprintf("%s/%s", owner, name)
	pipeline, _, err := client.Pipelines.ListProjectPipelines(pid, opts)
	if err != nil {
		return "", errors.New("unexpected: cannot get pipeline status")
	} else if len(pipeline) == 0 {
		return "No pipeline was found", nil
	}
	return pipeline[0].Status, nil
}