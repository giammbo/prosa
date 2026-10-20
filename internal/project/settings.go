package project

import "github.com/giammbo/prosa/internal/engine"

// Settings returns where hashicorp/aws operates: the region of
// ResolveRegion and, for an emulated AWS, the endpoint PROSA_AWS_ENDPOINT.
// The account and the credentials are the ones of the AWS CLI's chain;
// Prosa does not check the account (ADR-029).
func Settings(getenv func(string) string) (engine.AWSSettings, error) {
	region, err := ResolveRegion(getenv)
	if err != nil {
		return engine.AWSSettings{}, err
	}
	return engine.AWSSettings{Region: region, Endpoint: getenv("PROSA_AWS_ENDPOINT")}, nil
}
