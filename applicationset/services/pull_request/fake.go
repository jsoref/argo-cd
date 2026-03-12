package pull_request

import (
	"context"
)

type FakeService struct {
	listPullRequests []*PullRequest
	listError        error
}

var _ PullRequestService = (*FakeService)(nil)

func NewFakeService(_ context.Context, listPullRequests []*PullRequest, listError error) (PullRequestService, error) {
	return &FakeService{
		listPullRequests: listPullRequests,
		listError:        listError,
	}, nil
}

func (g *FakeService) List(_ context.Context) ([]*PullRequest, error) {
	return g.listPullRequests, g.listError
}
