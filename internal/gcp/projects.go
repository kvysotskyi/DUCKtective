package gcp

import (
	"context"
	"sort"

	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/option"
)

type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListProjects lists every ACTIVE project the ADC identity can see, for the wiretap-creation
// project picker — a GCS bucket lives in exactly one project, and that project isn't necessarily
// whatever `gcloud config` currently has active.
func ListProjects(ctx context.Context) ([]Project, error) {
	svc, err := cloudresourcemanager.NewService(ctx, option.WithScopes(cloudPlatformScope))
	if err != nil {
		return nil, err
	}

	var projects []Project
	call := svc.Projects.List()
	err = call.Pages(ctx, func(page *cloudresourcemanager.ListProjectsResponse) error {
		for _, p := range page.Projects {
			if p.LifecycleState != "ACTIVE" {
				continue
			}
			name := p.Name
			if name == "" {
				name = p.ProjectId
			}
			projects = append(projects, Project{ID: p.ProjectId, Name: name})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects, nil
}
