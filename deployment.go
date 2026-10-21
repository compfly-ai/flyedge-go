// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import "os"

type deploymentInfo struct {
	Platform string `json:"platform,omitempty"`
	Region   string `json:"region,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

// processDeployment detects the platform this process runs on.
func processDeployment() *deploymentInfo {
	_, err := os.Stat("/.dockerenv")
	return detectDeployment(os.Getenv, err == nil)
}

// detectDeployment reads deployment platform, region, and host from well-known platform env vars.
// Keep the order and values in step with the other SDKs (testdata/deployment_platforms.json).
func detectDeployment(getenv func(string) string, dockerenv bool) *deploymentInfo {
	set := func(k string) bool { return getenv(k) != "" }
	// Managed platforms first: some also set the generic signals checked last
	// (Container Apps may set KUBERNETES_SERVICE_HOST, Functions sets WEBSITE_SITE_NAME).
	var platform string
	switch {
	case set("AWS_LAMBDA_FUNCTION_NAME"):
		platform = "aws_lambda"
	case set("ECS_CONTAINER_METADATA_URI_V4") || set("ECS_CONTAINER_METADATA_URI"):
		platform = "aws_ecs"
	case set("K_SERVICE") && set("FUNCTION_TARGET"):
		platform = "cloud_run_functions"
	case set("K_SERVICE"):
		platform = "cloud_run"
	case set("CLOUD_RUN_JOB"):
		platform = "cloud_run_job"
	case set("FUNCTIONS_WORKER_RUNTIME"):
		platform = "azure_functions"
	case set("CONTAINER_APP_NAME"):
		platform = "azure_container_apps"
	case set("WEBSITE_SITE_NAME"):
		platform = "azure_app_service"
	case set("VERCEL"):
		platform = "vercel"
	case set("KUBERNETES_SERVICE_HOST"):
		platform = "kubernetes"
	case dockerenv:
		platform = "docker"
	}
	// Only alongside a platform: developer shells often export AWS_REGION/AWS_DEFAULT_REGION.
	var region string
	if platform != "" {
		for _, k := range []string{"AWS_REGION", "AWS_DEFAULT_REGION", "REGION_NAME", "VERCEL_REGION"} {
			if region = getenv(k); region != "" {
				break
			}
		}
	}
	hostname := getenv("HOSTNAME")
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	return &deploymentInfo{Platform: platform, Region: region, Hostname: hostname}
}
