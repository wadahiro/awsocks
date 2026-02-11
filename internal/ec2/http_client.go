package ec2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// HTTPClient implements EC2 API calls using direct HTTP requests
type HTTPClient struct {
	httpClient  *http.Client
	credentials aws.CredentialsProvider
	region      string
	signer      *v4.Signer
}

// NewHTTPClient creates a new HTTP-based EC2 client
func NewHTTPClient(cfg aws.Config) *HTTPClient {
	return &HTTPClient{
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		credentials: cfg.Credentials,
		region:      cfg.Region,
		signer:      v4.NewSigner(),
	}
}

// DescribeInstancesInput represents the input for DescribeInstances
type DescribeInstancesInput struct {
	InstanceIds []string
	Filters     []Filter
}

// Filter represents an EC2 filter
type Filter struct {
	Name   string
	Values []string
}

// DescribeInstancesOutput represents the output from DescribeInstances
type DescribeInstancesOutput struct {
	Reservations []Reservation
}

// Reservation represents an EC2 reservation
type Reservation struct {
	Instances []InstanceInfo
}

// InstanceInfo represents an EC2 instance
type InstanceInfo struct {
	InstanceId string
	State      InstanceState
	Tags       []Tag
}

// InstanceState represents the instance state
type InstanceState struct {
	Name string
}

// Tag represents an EC2 tag
type Tag struct {
	Key   string
	Value string
}

// StartInstancesInput represents the input for StartInstances
type StartInstancesInput struct {
	InstanceIds []string
}

// StartInstancesOutput represents the output from StartInstances
type StartInstancesOutput struct{}

// StopInstancesInput represents the input for StopInstances
type StopInstancesInput struct {
	InstanceIds []string
}

// StopInstancesOutput represents the output from StopInstances
type StopInstancesOutput struct{}

// DescribeInstances calls the EC2 DescribeInstances API
func (c *HTTPClient) DescribeInstances(ctx context.Context, input *DescribeInstancesInput) (*DescribeInstancesOutput, error) {
	params := url.Values{}
	params.Set("Action", "DescribeInstances")
	params.Set("Version", "2016-11-15")

	// Add instance IDs
	for i, id := range input.InstanceIds {
		params.Set(fmt.Sprintf("InstanceId.%d", i+1), id)
	}

	// Add filters
	for i, filter := range input.Filters {
		params.Set(fmt.Sprintf("Filter.%d.Name", i+1), filter.Name)
		for j, value := range filter.Values {
			params.Set(fmt.Sprintf("Filter.%d.Value.%d", i+1, j+1), value)
		}
	}

	body, err := c.doRequest(ctx, params)
	if err != nil {
		return nil, err
	}

	var xmlResp describeInstancesXMLResponse
	if err := xml.Unmarshal(body, &xmlResp); err != nil {
		return nil, fmt.Errorf("failed to parse DescribeInstances response: %w", err)
	}

	return xmlResp.toOutput(), nil
}

// StartInstances calls the EC2 StartInstances API
func (c *HTTPClient) StartInstances(ctx context.Context, input *StartInstancesInput) (*StartInstancesOutput, error) {
	params := url.Values{}
	params.Set("Action", "StartInstances")
	params.Set("Version", "2016-11-15")

	for i, id := range input.InstanceIds {
		params.Set(fmt.Sprintf("InstanceId.%d", i+1), id)
	}

	_, err := c.doRequest(ctx, params)
	if err != nil {
		return nil, err
	}

	return &StartInstancesOutput{}, nil
}

// StopInstances calls the EC2 StopInstances API
func (c *HTTPClient) StopInstances(ctx context.Context, input *StopInstancesInput) (*StopInstancesOutput, error) {
	params := url.Values{}
	params.Set("Action", "StopInstances")
	params.Set("Version", "2016-11-15")

	for i, id := range input.InstanceIds {
		params.Set(fmt.Sprintf("InstanceId.%d", i+1), id)
	}

	_, err := c.doRequest(ctx, params)
	if err != nil {
		return nil, err
	}

	return &StopInstancesOutput{}, nil
}

func (c *HTTPClient) doRequest(ctx context.Context, params url.Values) ([]byte, error) {
	endpoint := fmt.Sprintf("https://ec2.%s.amazonaws.com/", c.region)

	// Sort parameters for consistent signing
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sortedParams []string
	for _, k := range keys {
		for _, v := range params[k] {
			sortedParams = append(sortedParams, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	body := strings.Join(sortedParams, "&")

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBufferString(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// Get credentials and sign the request
	creds, err := c.credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve credentials: %w", err)
	}

	// Calculate payload hash
	payloadHash := sha256.Sum256([]byte(body))
	payloadHashHex := hex.EncodeToString(payloadHash[:])

	err = c.signer.SignHTTP(ctx, creds, req, payloadHashHex, "ec2", c.region, time.Now())
	if err != nil {
		return nil, fmt.Errorf("failed to sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Parse error response
		var errorResp ec2ErrorResponse
		if xmlErr := xml.Unmarshal(respBody, &errorResp); xmlErr == nil && errorResp.Errors.Error.Code != "" {
			return nil, fmt.Errorf("EC2 API error: %s - %s", errorResp.Errors.Error.Code, errorResp.Errors.Error.Message)
		}
		return nil, fmt.Errorf("EC2 API error: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// XML response structures for parsing

type describeInstancesXMLResponse struct {
	XMLName        xml.Name `xml:"DescribeInstancesResponse"`
	ReservationSet struct {
		Items []reservationXML `xml:"item"`
	} `xml:"reservationSet"`
}

type reservationXML struct {
	InstancesSet struct {
		Items []instanceXML `xml:"item"`
	} `xml:"instancesSet"`
}

type instanceXML struct {
	InstanceId    string `xml:"instanceId"`
	InstanceState struct {
		Name string `xml:"name"`
	} `xml:"instanceState"`
	TagSet struct {
		Items []tagXML `xml:"item"`
	} `xml:"tagSet"`
}

type tagXML struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

func (r *describeInstancesXMLResponse) toOutput() *DescribeInstancesOutput {
	output := &DescribeInstancesOutput{}
	for _, resItem := range r.ReservationSet.Items {
		reservation := Reservation{}
		for _, instItem := range resItem.InstancesSet.Items {
			inst := InstanceInfo{
				InstanceId: instItem.InstanceId,
				State: InstanceState{
					Name: instItem.InstanceState.Name,
				},
			}
			for _, tagItem := range instItem.TagSet.Items {
				inst.Tags = append(inst.Tags, Tag{
					Key:   tagItem.Key,
					Value: tagItem.Value,
				})
			}
			reservation.Instances = append(reservation.Instances, inst)
		}
		output.Reservations = append(output.Reservations, reservation)
	}
	return output
}

type ec2ErrorResponse struct {
	XMLName xml.Name `xml:"Response"`
	Errors  struct {
		Error struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
	} `xml:"Errors"`
}
