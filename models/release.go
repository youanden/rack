package models

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/convox/kernel/Godeps/_workspace/src/github.com/awslabs/aws-sdk-go/aws"
	"github.com/convox/kernel/Godeps/_workspace/src/github.com/awslabs/aws-sdk-go/service/cloudformation"
	"github.com/convox/kernel/Godeps/_workspace/src/github.com/awslabs/aws-sdk-go/service/dynamodb"
	"github.com/convox/kernel/Godeps/_workspace/src/github.com/awslabs/aws-sdk-go/service/ecs"
)

type Release struct {
	Id string

	Cluster string
	App     string

	Active   bool
	Build    string
	Env      string
	Manifest string
	Tasks    map[string]string

	Created time.Time
}

type Releases []Release

func NewRelease(cluster, app string) Release {
	return Release{
		Id:      generateId("R", 10),
		Cluster: cluster,
		App:     app,
	}
}

func ListReleases(cluster, app string) (Releases, error) {
	req := &dynamodb.QueryInput{
		KeyConditions: &map[string]*dynamodb.Condition{
			"app": &dynamodb.Condition{
				AttributeValueList: []*dynamodb.AttributeValue{
					&dynamodb.AttributeValue{S: aws.String(app)},
				},
				ComparisonOperator: aws.String("EQ"),
			},
		},
		IndexName:        aws.String("app.created"),
		Limit:            aws.Long(10),
		ScanIndexForward: aws.Boolean(false),
		TableName:        aws.String(releasesTable(cluster, app)),
	}

	a, err := GetApp(cluster, app)

	if err != nil {
		return nil, err
	}

	res, err := DynamoDB().Query(req)

	if err != nil {
		return nil, err
	}

	releases := make(Releases, len(res.Items))

	for i, item := range res.Items {
		releases[i] = *releaseFromItem(*item)
		releases[i].Active = (a.Release == releases[i].Id)
	}

	return releases, nil
}

func GetRelease(cluster, app, id string) (*Release, error) {
	req := &dynamodb.GetItemInput{
		ConsistentRead: aws.Boolean(true),
		Key: &map[string]*dynamodb.AttributeValue{
			"id": &dynamodb.AttributeValue{S: aws.String(id)},
		},
		TableName: aws.String(releasesTable(cluster, app)),
	}

	a, err := GetApp(cluster, app)

	if err != nil {
		return nil, err
	}

	res, err := DynamoDB().GetItem(req)

	if err != nil {
		return nil, err
	}

	release := releaseFromItem(*res.Item)
	release.Active = (a.Release == release.Id)

	return release, nil
}

func (r *Release) Cleanup() error {
	app, err := GetApp(r.Cluster, r.App)

	if err != nil {
		return err
	}

	// delete env
	err = s3Delete(app.Outputs["Settings"], fmt.Sprintf("releases/%s/env", r.Id))

	if err != nil {
		return err
	}

	return nil
}

func (r *Release) Save() error {
	if r.Id == "" {
		return fmt.Errorf("Id must not be blank")
	}

	if r.Created.IsZero() {
		r.Created = time.Now()
	}

	err := r.registerTasks()

	if err != nil {
		return err
	}

	req := &dynamodb.PutItemInput{
		Item: &map[string]*dynamodb.AttributeValue{
			"id":      &dynamodb.AttributeValue{S: aws.String(r.Id)},
			"cluster": &dynamodb.AttributeValue{S: aws.String(r.Cluster)},
			"app":     &dynamodb.AttributeValue{S: aws.String(r.App)},
			"created": &dynamodb.AttributeValue{S: aws.String(r.Created.Format(SortableTime))},
		},
		TableName: aws.String(releasesTable(r.Cluster, r.App)),
	}

	if r.Build != "" {
		(*req.Item)["build"] = &dynamodb.AttributeValue{S: aws.String(r.Build)}
	}

	if r.Env != "" {
		(*req.Item)["env"] = &dynamodb.AttributeValue{S: aws.String(r.Env)}
	}

	if r.Manifest != "" {
		(*req.Item)["manifest"] = &dynamodb.AttributeValue{S: aws.String(r.Manifest)}
	}

	tasks, err := json.Marshal(r.Tasks)

	if err != nil {
		return err
	}

	(*req.Item)["tasks"] = &dynamodb.AttributeValue{S: aws.String(string(tasks))}

	_, err = DynamoDB().PutItem(req)

	if err != nil {
		return err
	}

	return nil
}

func (r *Release) Promote() error {
	formation, err := r.Formation()

	if err != nil {
		return err
	}

	existing, err := formationParameters(formation)

	if err != nil {
		return err
	}

	app, err := GetApp(r.Cluster, r.App)

	if err != nil {
		return err
	}

	params := []*cloudformation.Parameter{}

	for key, value := range app.Parameters {
		if _, ok := existing[key]; ok {
			params = append(params, &cloudformation.Parameter{ParameterKey: aws.String(key), ParameterValue: aws.String(value)})
		}
	}

	req := &cloudformation.UpdateStackInput{
		StackName:    aws.String(fmt.Sprintf("%s-%s", r.Cluster, r.App)),
		TemplateBody: aws.String(formation),
		Parameters:   params,
	}

	_, err = CloudFormation().UpdateStack(req)

	fmt.Printf("err %+v\n", err)

	// TODO: wait for stack

	r.registerServices()

	return err
}

func (r *Release) Formation() (string, error) {
	processes, err := r.Processes()

	args := []string{"run", "convox/app"}

	for _, ps := range processes {
		for i, _ := range ps.Ports {
			// TODO fix base port
			args = append(args, "-p", fmt.Sprintf("%d:%d", 8000+i, 8000+i))
		}
	}

	data, err := exec.Command("docker", args...).CombinedOutput()

	if err != nil {
		return "", err
	}

	return string(data), nil
}

func (r *Release) Processes() (Processes, error) {
	manifest, err := LoadManifest(r.Manifest)

	if err != nil {
		return nil, err
	}

	return manifest.Processes(), nil
}

func (r *Release) Services() (Services, error) {
	manifest, err := LoadManifest(r.Manifest)

	if err != nil {
		return nil, err
	}

	services := manifest.Services()

	for i := range services {
		services[i].App = r.App
	}

	return services, nil
}

func (r *Release) registerServices() error {
	app, err := GetApp(r.Cluster, r.App)

	if err != nil {
		return err
	}

	pss, err := r.Processes()

	if err != nil {
		return err
	}

	for _, ps := range pss {
		gres, err := ECS().DescribeServices(&ecs.DescribeServicesInput{
			Cluster:  aws.String(r.Cluster),
			Services: []*string{aws.String(fmt.Sprintf("%s-%s-%s", r.Cluster, r.App, ps.Name))},
		})

		if err != nil {
			return err
		}

		fmt.Printf("r.Tasks %+v\n", r.Tasks)

		if len(gres.Services) < 1 {
			creq := &ecs.CreateServiceInput{
				Cluster:        aws.String(r.Cluster),
				DesiredCount:   aws.Long(int64(ps.Count)),
				Role:           aws.String("arn:aws:iam::778743527532:role/ecsServiceRole"),
				ServiceName:    aws.String(fmt.Sprintf("%s-%s-%s", r.Cluster, r.App, ps.Name)),
				TaskDefinition: aws.String(r.Tasks[ps.Name]),
			}

			for _, port := range ps.Ports {
				fmt.Printf("port %+v\n", port)
				creq.LoadBalancers = append(creq.LoadBalancers, &ecs.LoadBalancer{
					ContainerName:    aws.String("main"),
					ContainerPort:    aws.Long(int64(port)),
					LoadBalancerName: aws.String(app.Outputs["Balancer"]),
				})
			}

			cres, err := ECS().CreateService(creq)

			fmt.Printf("cres %+v\n", cres)
			fmt.Printf("err %+v\n", err)
		} else {
		}

		fmt.Printf("gres %+v\n", gres)
	}

	return nil
}

func (r *Release) registerTasks() error {
	tasks := map[string]string{}

	pss, err := r.Processes()

	if err != nil {
		return err
	}

	for _, ps := range pss {
		build, err := GetBuild(r.Cluster, r.App, r.Build)

		req := &ecs.RegisterTaskDefinitionInput{
			ContainerDefinitions: []*ecs.ContainerDefinition{
				{
					CPU:       aws.Long(200),
					Essential: aws.Boolean(true),
					Image:     aws.String(build.Image(ps.Name)),
					Memory:    aws.Long(300),
					Name:      aws.String("main"),
				},
			},
			Family: aws.String(fmt.Sprintf("%s-%s-%s", r.Cluster, r.App, ps.Name)),
		}

		if ps.Command != "" {
			req.ContainerDefinitions[0].Command = []*string{aws.String("sh"), aws.String("-c"), aws.String(ps.Command)}
		}

		// set environment
		env := LoadEnvironment([]byte(r.Env))

		for key, val := range env {
			req.ContainerDefinitions[0].Environment = append(req.ContainerDefinitions[0].Environment, &ecs.KeyValuePair{
				Name:  aws.String(key),
				Value: aws.String(val),
			})
		}

		// set portmappings
		// TODO: fix base port
		for i, p := range ps.Ports {
			req.ContainerDefinitions[0].PortMappings = append(req.ContainerDefinitions[0].PortMappings, &ecs.PortMapping{
				ContainerPort: aws.Long(int64(p)),
				HostPort:      aws.Long(int64(8000 + i)),
			})
		}

		res, err := ECS().RegisterTaskDefinition(req)

		if err != nil {
			return err
		}

		tasks[ps.Name] = fmt.Sprintf("%s:%d", *res.TaskDefinition.Family, *res.TaskDefinition.Revision)
	}

	r.Tasks = tasks

	return nil
}

func releasesTable(cluster, app string) string {
	return fmt.Sprintf("%s-%s-releases", cluster, app)
}

func releaseFromItem(item map[string]*dynamodb.AttributeValue) *Release {
	created, _ := time.Parse(SortableTime, coalesce(item["created"], ""))

	release := &Release{
		Id:       coalesce(item["id"], ""),
		Cluster:  coalesce(item["cluster"], ""),
		App:      coalesce(item["app"], ""),
		Build:    coalesce(item["build"], ""),
		Env:      coalesce(item["env"], ""),
		Manifest: coalesce(item["manifest"], ""),
		Created:  created,
	}

	var tasks map[string]string
	json.Unmarshal([]byte(coalesce(item["tasks"], "{}")), &tasks)
	release.Tasks = tasks

	return release
}
