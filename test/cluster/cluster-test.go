package main

import (
	"fmt"
	"log"

	container "cloud.google.com/go/container/apiv1"
	"golang.org/x/net/context"
	containerpb "google.golang.org/genproto/googleapis/container/v1"
)

const CIcluster = "default-buildkite"
const SRC_CLUSTER_K8S_QA_CREDENTIALS = ""

func main() {

	// access cluster
	ctx := context.Background()

	// TODO: Use secret in buildkite env
	//c, err := container.NewClusterManagerClient(ctx, option.WithCredentialsFile())
	c, err := container.NewClusterManagerClient(ctx)
	if err != nil {
		log.Fatal("unable to create cluster manager")
	}
	req := &containerpb.ListClustersRequest{
		Parent: "projects/sourcegraph-ci/locations/-",
	}

	resp, err := c.ListClusters(ctx, req)
	if err != nil {
		log.Fatal("Unable to list clusters:", err)
	}

	var targetCluster containerpb.Cluster

	for _, cluster := range resp.Clusters {
		if cluster.Name == CIcluster {
			targetCluster = *cluster
		}
	}

	r := &containerpb.GetClusterRequest{
		Name: "projects/sourcegraph-ci/locations/us-central1-c/clusters/default-buildkite",
	}
	config, err := c.GetCluster(ctx, r)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(config)

	// deploy sourcegraph

	// run some tests /

}

//func BuildClusterConfig(ctx context.Context, token string, project string, zone string,
//	clusterID string) (*rest.Config, error) {
//	ts := oauth2.StaticTokenSource(&oauth2.Token{
//		AccessToken: token,
//	})
//	c, err := container.NewClusterManagerClient(ctx, option.WithTokenSource(ts))
//	if err != nil {
//		return nil, err
//	}
//	req := &containerpb.GetClusterRequest{
//		ProjectId: project,
//	}
//	resp, err := c.GetCluster(ctx, req)
//	if err != nil {
//		return nil, err
//	}
//	caDec, _ := base64.StdEncoding.DecodeString(resp.MasterAuth.ClusterCaCertificate)
//	return &rest.Config{
//		Host:        "https://" + resp.Endpoint,
//		BearerToken: token,
//		TLSClientConfig: rest.TLSClientConfig{
//			CAData: []byte(string(caDec)),
//		},
//	}, nil
//}
