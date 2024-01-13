package azure

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/opencost/opencost/pkg/cloud"
	"github.com/opencost/opencost/pkg/env"
	"github.com/opencost/opencost/pkg/log"
)

// AzureStorageBillingParser accesses billing data stored in CSV files in Azure Storage
type AzureStorageBillingParser struct {
	StorageConnection
}

func (asbp *AzureStorageBillingParser) Equals(config cloud.Config) bool {
	thatConfig, ok := config.(*AzureStorageBillingParser)
	if !ok {
		return false
	}
	return asbp.StorageConnection.Equals(&thatConfig.StorageConnection)
}

type AzureBillingResultFunc func(*BillingRowValues) error

func (asbp *AzureStorageBillingParser) ParseBillingData(start, end time.Time, resultFn AzureBillingResultFunc) error {
	err := asbp.Validate()
	if err != nil {
		asbp.ConnectionStatus = cloud.InvalidConfiguration
		return err
	}
	serviceURL := fmt.Sprintf(asbp.StorageConnection.getBlobURLTemplate(), asbp.Account, "")
	client, err := asbp.Authorizer.GetBlobClient(serviceURL)
	if err != nil {
		asbp.ConnectionStatus = cloud.FailedConnection
		return err
	}
	ctx := context.Background()

	// ----- HARDCODED BILLING DATA -----
	// blobNames, err := asbp.getMostRecentBlobs(start, end, client, ctx)
	// if err != nil {
	// 	asbp.ConnectionStatus = cloud.FailedConnection
	// 	return err
	// }
	// if len(blobNames) == 0 && asbp.ConnectionStatus != cloud.SuccessfulConnection {
	// 	asbp.ConnectionStatus = cloud.MissingData
	// 	return nil
	// }

	// log.Infof("blobNames: %v", blobNames)  // [export/thomasnExport/20240101-20240131/thomasnExport_758a42af-0731-4edb-b498-1e523bb40f12.csv]

	blobNames := []string{"thomasn-scale-test.csv"}
	// ----- HARDCODED BILLING DATA -----

	for _, blobName := range blobNames {
		// ----- INSTRUMENTATION -----
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		log.Infof("ParseBillingData: HeapAlloc:  %v MB", m.HeapAlloc/1024/1024)
		// ----- INSTRUMENTATION -----

		if env.IsAzureParseBillingPaginated() {
			localFilePath := filepath.Join(env.GetConfigPathWithDefault(env.DefaultConfigMountPath), "db", "cloudCost", "azurebilling.csv")
			err := asbp.DownloadBlobToFile(localFilePath, blobName, client, ctx)
			if err != nil {
				asbp.ConnectionStatus = cloud.FailedConnection
				return err
			}
			fp, err := os.Open(localFilePath)
			if err != nil {
				asbp.ConnectionStatus = cloud.FailedConnection
				return err
			}
			defer fp.Close()
			err = asbp.parseCSV(start, end, csv.NewReader(fp), resultFn)
			if err != nil {
				asbp.ConnectionStatus = cloud.ParseError
				return err
			}
		} else {
			blobBytes, err2 := asbp.DownloadBlob(blobName, client, ctx)
			if err2 != nil {
				asbp.ConnectionStatus = cloud.FailedConnection
				return err2
			}
			err2 = asbp.parseCSV(start, end, csv.NewReader(bytes.NewReader(blobBytes)), resultFn)
			if err2 != nil {
				asbp.ConnectionStatus = cloud.ParseError
				return err2
			}
		}
	}
	asbp.ConnectionStatus = cloud.SuccessfulConnection
	return nil
}

func (asbp *AzureStorageBillingParser) parseCSV(start, end time.Time, reader *csv.Reader, resultFn AzureBillingResultFunc) error {
	// ----- INSTRUMENTATION -----
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	log.Infof("parseCSV: HeapAlloc:  %v MB", m.HeapAlloc/1024/1024)
	// ----- INSTRUMENTATION -----

	headers, err := reader.Read()
	if err != nil {
		return err
	}
	abp, err := NewBillingParseSchema(headers)
	if err != nil {
		return err
	}
	for {
		var record, err = reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		abv := abp.ParseRow(start, end, record)
		if abv == nil {
			continue
		}

		err = resultFn(abv)
		if err != nil {
			return err
		}
	}

	// ----- INSTRUMENTATION -----
	runtime.ReadMemStats(&m)
	log.Infof("parseCSV: HeapAlloc:  %v MB", m.HeapAlloc/1024/1024)
	// ----- INSTRUMENTATION -----

	return nil
}

func (asbp *AzureStorageBillingParser) getMostRecentBlobs(start, end time.Time, client *azblob.Client, ctx context.Context) ([]string, error) {
	log.Infof("Azure Storage: retrieving most recent reports from: %v - %v", start, end)

	// Get list of month substrings for months contained in the start to end range
	monthStrs, err := asbp.getMonthStrings(start, end)
	if err != nil {
		return nil, err
	}
	mostResentBlobs := make(map[string]container.BlobItem)

	pager := client.NewListBlobsFlatPager(asbp.Container, &azblob.ListBlobsFlatOptions{
		Include: container.ListBlobsInclude{Deleted: false, Versions: false},
	})

	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		// Using the list of months strings find the most resent blob for each month in the range
		for _, blobInfo := range resp.Segment.BlobItems {
			if blobInfo.Name == nil {
				continue
			}
			// If Container Path configuration exists, check if it is in the blobs name
			if asbp.Path != "" && !strings.Contains(*blobInfo.Name, asbp.Path) {
				continue
			}
			for _, month := range monthStrs {
				if strings.Contains(*blobInfo.Name, month) {
					// check if blob is the newest seen for this month
					if prevBlob, ok := mostResentBlobs[month]; ok {
						if prevBlob.Properties.CreationTime.After(*blobInfo.Properties.CreationTime) {
							continue
						}
					}
					mostResentBlobs[month] = *blobInfo
				}
			}
		}
	}

	// convert blob names into blob urls and move from map into ordered list of blob names
	var blobNames []string
	for _, month := range monthStrs {
		if blob, ok := mostResentBlobs[month]; ok {
			blobNames = append(blobNames, *blob.Name)
		}
	}

	return blobNames, nil
}

func (asbp *AzureStorageBillingParser) getMonthStrings(start, end time.Time) ([]string, error) {
	if start.After(end) {
		return []string{}, fmt.Errorf("start date must be before end date")
	}
	if end.After(time.Now()) {
		end = time.Now()
	}
	var monthStrs []string
	monthStr := asbp.timeToMonthString(start)
	endStr := asbp.timeToMonthString(end)
	monthStrs = append(monthStrs, monthStr)
	currMonth := start.AddDate(0, 0, -start.Day()+1)
	for monthStr != endStr {
		currMonth = currMonth.AddDate(0, 1, 0)
		monthStr = asbp.timeToMonthString(currMonth)
		monthStrs = append(monthStrs, monthStr)
	}

	return monthStrs, nil
}

func (asbp *AzureStorageBillingParser) timeToMonthString(input time.Time) string {
	format := "20060102"
	startOfMonth := input.AddDate(0, 0, -input.Day()+1)
	endOfMonth := input.AddDate(0, 1, -input.Day())
	return startOfMonth.Format(format) + "-" + endOfMonth.Format(format)
}
