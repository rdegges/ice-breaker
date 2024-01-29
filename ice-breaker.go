package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/glacier"
	"github.com/aws/aws-sdk-go-v2/service/glacier/types"
)

const (
	colorRed        = "\033[31m"
	colorGreen      = "\033[32m"
	colorYellow     = "\033[33m"
	colorReset      = "\033[0m"
	boldText        = "\033[1m"
	pollingInterval = 1 * time.Minute
)

var awsRegions = []string{
	"us-east-2", "us-east-1", "us-west-1", "us-west-2", "af-south-1",
	"ap-east-1", "ap-southeast-3", "ap-south-1", "ap-northeast-3", "ap-northeast-2",
	"ap-southeast-1", "ap-southeast-2", "ap-northeast-1", "ca-central-1",
	"eu-central-1", "eu-west-1", "eu-west-2", "eu-south-1", "eu-west-3",
	"eu-north-1", "me-south-1", "sa-east-1", "us-gov-east-1", "us-gov-west-1",
}

type InventoryJobOutput struct {
	ArchiveList []struct {
		ArchiveId string `json:"ArchiveId"`
	} `json:"ArchiveList"`
}

type Glacier struct {
	Context context.Context
	Client  *glacier.Client
	Region  string
}

type Vault struct {
	Glacier *Glacier
	Name    string
}

type InventoryJob struct {
	Vault *Vault
	Id    string
}

type Archive struct {
	Vault *Vault
	Id    string
}

func (a *Archive) Delete() error {
	_, err := a.Vault.Glacier.Client.DeleteArchive(a.Vault.Glacier.Context, &glacier.DeleteArchiveInput{
		VaultName: aws.String(a.Vault.Name),
		ArchiveId: aws.String(a.Id),
	})

	if err != nil {
		return fmt.Errorf("failed to delete archive: %v", err)
	}

	fmt.Printf("Archive %s successfully deleted from vault %s\n", a.Id, a.Vault.Name)
	return nil
}

func (g *Glacier) New(region string, accessKeyID, secretAccessKey string) error {
	if g.Context == nil {
		g.Context = context.TODO()
	}

	cfg, err := config.LoadDefaultConfig(g.Context,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")),
	)
	if err != nil {
		return err
	}

	g.Client = glacier.NewFromConfig(cfg)
	g.Region = region
	return nil
}

func (g *Glacier) GetVaults() (*[]*Vault, error) {
	output, err := g.Client.ListVaults(g.Context, &glacier.ListVaultsInput{})
	if err != nil {
		return nil, fmt.Errorf("error listing Glacier vaults in region %s: %w", g.Region, err)
	}

	var vaults []*Vault
	for _, vault := range output.VaultList {
		vaults = append(vaults, &Vault{g, *vault.VaultName})
	}

	return &vaults, nil
}

func (v *Vault) InitiateInventoryRetrievalJob() (*InventoryJob, error) {
	params := &glacier.InitiateJobInput{
		AccountId: aws.String("-"), // Use "-" for the current account
		VaultName: aws.String(v.Name),
		JobParameters: &types.JobParameters{
			Type: aws.String("inventory-retrieval"),
		},
	}

	result, err := v.Glacier.Client.InitiateJob(v.Glacier.Context, params)
	if err != nil {
		return &InventoryJob{}, fmt.Errorf("failed to initiate inventory retrieval job: %w", err)
	}
	return &InventoryJob{v, *result.JobId}, nil
}

func (j *InventoryJob) GetResults() (*[]*Archive, error) {
	output, err := j.Vault.Glacier.Client.GetJobOutput(j.Vault.Glacier.Context, &glacier.GetJobOutputInput{
		JobId:     aws.String(j.Id),
		VaultName: aws.String(j.Vault.Name),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get job output: %w", err)
	}

	defer output.Body.Close()

	var jobOutput InventoryJobOutput
	if err := json.NewDecoder(output.Body).Decode(&jobOutput); err != nil {
		return nil, fmt.Errorf("failed to decode job output: %w", err)
	}

	var archives []*Archive
	for _, archive := range jobOutput.ArchiveList {
		archives = append(archives, &Archive{j.Vault, archive.ArchiveId})
	}

	return &archives, nil
}

func (v *Vault) getArchives() error {
	job, err := v.InitiateInventoryRetrievalJob()
	if err != nil {
		return fmt.Errorf("failed to initiate inventory retrieval job: %w", err)
	}

	log.Printf("Inventory retrieval job initiated for vault %s, job ID: %s\n%s%sThis operation will likely take a number of hours to complete. Please wait while AWS generates a list of archives for this vault.%s", v.Name, job.Id, colorYellow, boldText, colorReset)

	// Wait for the job to complete
	for {
		select {
		case <-v.Glacier.Context.Done():
			return v.Glacier.Context.Err()
		case <-time.After(pollingInterval):
			description, err := v.Glacier.Client.DescribeJob(v.Glacier.Context, &glacier.DescribeJobInput{
				JobId:     aws.String(job.Id),
				VaultName: aws.String(v.Name),
			})
			if err != nil {
				return fmt.Errorf("failed to describe job: %w", err)
			}

			if description.Completed {
				log.Println("Inventory retrieval job completed")

				// Get the job results
				archives, err := job.GetResults()
				if err != nil {
					return fmt.Errorf("failed to get inventory job results: %w", err)
				}

				// Print archive IDs
				for i, archive := range *archives {
					fmt.Println(i, "Archive ID:", archive.Id)
					if err := archive.Delete(); err != nil {
						fmt.Printf("Error deleting archive: %v\n", err)
					}
				}

				return nil
			} else {
				log.Println("Waiting for inventory retrieval job to complete")
			}
		}
	}
}

func main() {
	accessKeyID := flag.String("id", "", "AWS Access Key ID")
	secretAccessKey := flag.String("secret", "", "AWS Secret Access Key")
	region := flag.String("region", "", "AWS Region")

	flag.Parse()

	if *accessKeyID == "" || *secretAccessKey == "" {
		log.Fatal("AWS Access Key ID and Secret Access Key are required")
	}

	if *region != "" {
		awsRegions = []string{*region}
	}

	for _, region := range awsRegions {
		fmt.Printf("Scanning for Glacier Vaults in region %s%s%s%s\n", colorGreen, boldText, region, colorReset)

		g := &Glacier{}
		if err := g.New(region, *accessKeyID, *secretAccessKey); err != nil {
			fmt.Printf("Error creating Glacier client for region %s: %v", region, err)
		}

		vaults, err := g.GetVaults()
		if err != nil {
			fmt.Printf("%sSkipping region %s: %v%s", colorYellow, g.Region, err, colorReset)
			continue
		}

		reader := bufio.NewReader(os.Stdin)
		for _, vault := range *vaults {
			fmt.Printf("%s%s[%s] %s: Would you like to destroy this vault? (y/N) %s", boldText, colorRed, vault.Glacier.Region, vault.Name, colorReset)
			response, _ := reader.ReadString('\n')
			if strings.TrimSpace(strings.ToLower(response)) == "y" {
				fmt.Printf("%sVault %s in region %s marked for deletion.%s\n", colorGreen, vault.Name, vault.Glacier.Region, colorReset)
				g := &Glacier{}
				if err := g.New(vault.Glacier.Region, *accessKeyID, *secretAccessKey); err != nil {
					fmt.Printf("Error creating Glacier client for region %s: %v\n", vault.Glacier.Region, err)

				}

				vault.getArchives()
			}
		}
	}
}
