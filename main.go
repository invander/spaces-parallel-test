package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3Config struct {
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	Bucket    string
}

type Stat struct {
	Duration time.Duration
	Success  bool
}

type JSONLog struct {
	Time     string `json:"time"`
	Level    string `json:"level"`
	Message  string `json:"message"`
	ExtraKey string `json:"key,omitempty"`
	TaskID   string `json:"task_id,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

func jsonLog(level, msg, taskID string, fileSize int64, key ...string) {
	logEntry := JSONLog{
		Time:     time.Now().UTC().Format(time.RFC3339),
		Level:    level,
		Message:  msg,
		TaskID:   taskID,
		FileSize: fileSize,
	}
	if len(key) > 0 {
		logEntry.ExtraKey = key[0]
	}
	data, _ := json.Marshal(logEntry)
	fmt.Println(string(data))
}

func NewS3Client(ctx context.Context, conf S3Config) (*s3.Client, error) {
	cfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion(conf.Region),
		config.WithCredentialsProvider(aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     conf.AccessKey,
				SecretAccessKey: conf.SecretKey,
			}, nil
		})),
	)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.EndpointResolver = s3.EndpointResolverFunc(func(region string, options s3.EndpointResolverOptions) (aws.Endpoint, error) {
			return aws.Endpoint{
				URL:           conf.Endpoint,
				SigningRegion: "us-east-1",
			}, nil
		})
	})

	return client, nil
}

func downloadTestFile(url string) ([]byte, int64, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to download file: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return data, int64(len(data)), nil
}

func uploadFile(ctx context.Context, client *s3.Client, bucket, key string, data []byte) error {
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   bytes.NewReader(data),
	})
	return err
}

func waitForObject(ctx context.Context, client *s3.Client, bucket, key string, timeout time.Duration) (time.Duration, error) {
	waiter := s3.NewObjectExistsWaiter(client)
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	err := waiter.Wait(waitCtx, &s3.HeadObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}, timeout)
	return time.Since(start), err
}

func deleteObject(ctx context.Context, client *s3.Client, bucket, key string) error {
	_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	return err
}

func setupLogFile(logPath string) (*os.File, error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	mw := io.MultiWriter(os.Stdout, f)
	log.SetOutput(mw)
	log.SetFlags(0)
	return f, nil
}

func randomTestFileURL() string {
	urls := []string{
		"https://proof.ovh.net/files/10Mb.dat",
		"https://proof.ovh.net/files/100Mb.dat",
	}
	rand.Seed(time.Now().UnixNano())
	return urls[rand.Intn(len(urls))]
}

func runTest(ctx context.Context, i int, client *s3.Client, conf S3Config, data []byte, fileSize int64, statsChan chan<- Stat, wg *sync.WaitGroup, sem chan struct{}) {
	taskID := strconv.Itoa(i)
	defer wg.Done()
	sem <- struct{}{}
	defer func() { <-sem }()

	key := fmt.Sprintf("test-object-%d-%d.bin", time.Now().UnixNano(), i)
	jsonLog("info", fmt.Sprintf("Run #%d: Uploading %s", i, key), taskID, fileSize, key)

	if err := uploadFile(ctx, client, conf.Bucket, key, data); err != nil {
		jsonLog("error", fmt.Sprintf("Upload failed: %v", err), taskID, fileSize, key)
		statsChan <- Stat{Success: false}
		return
	}
	jsonLog("info", "Upload complete", taskID, fileSize, key)

	jsonLog("info", "Waiting for object...", taskID, fileSize, key)
	elapsed, err := waitForObject(ctx, client, conf.Bucket, key, 10*time.Second)
	if err != nil {
		jsonLog("warn", fmt.Sprintf("Object not available after %s: %v", elapsed, err), taskID, fileSize, key)
		statsChan <- Stat{Duration: elapsed, Success: false}
	} else {
		jsonLog("info", fmt.Sprintf("Object appeared after %s", elapsed), taskID, fileSize, key)
		statsChan <- Stat{Duration: elapsed, Success: true}
	}

	if err := deleteObject(ctx, client, conf.Bucket, key); err != nil {
		jsonLog("warn", fmt.Sprintf("Failed to delete object: %v", err), taskID, fileSize, key)
	} else {
		jsonLog("info", "Object deleted", taskID, fileSize, key)
	}
}

func main() {
	var count int
	var parallel int
	var logPath string
	flag.IntVar(&count, "count", 10, "Number of iterations (min 10)")
	flag.IntVar(&parallel, "parallel", 1, "Number of parallel workers")
	flag.StringVar(&logPath, "logfile", "spaces_test.log", "Path to log file")
	flag.Parse()

	if count < 10 {
		count = 10
	}
	if parallel < 1 {
		parallel = 1
	}

	logFile, err := setupLogFile(logPath)
	if err != nil {
		log.Fatalf("Failed to set up log file: %v", err)
	}
	defer logFile.Close()

	ctx := context.Background()
	conf := S3Config{
		Endpoint:  os.Getenv("SPACES_ENDPOINT"),
		Region:    os.Getenv("SPACES_REGION"),
		AccessKey: os.Getenv("SPACES_KEY"),
		SecretKey: os.Getenv("SPACES_SECRET"),
		Bucket:    os.Getenv("SPACES_BUCKET"),
	}

	if conf.Endpoint == "" || conf.Region == "" || conf.AccessKey == "" || conf.SecretKey == "" || conf.Bucket == "" {
		log.Fatalln("Missing one or more required environment variables: SPACES_ENDPOINT, SPACES_REGION, SPACES_KEY, SPACES_SECRET, SPACES_BUCKET")
	}

	client, err := NewS3Client(ctx, conf)
	if err != nil {
		log.Fatalf("failed to create S3 client: %v", err)
	}

	statsChan := make(chan Stat, count)
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallel)

	for i := 1; i <= count; i++ {
		url := randomTestFileURL()
		data, fileSize, err := downloadTestFile(url)
		if err != nil {
			log.Fatalf("failed to download test file: %v", err)
		}
		wg.Add(1)
		go runTest(ctx, i, client, conf, data, fileSize, statsChan, &wg, sem)
	}

	wg.Wait()
	close(statsChan)

	var stats []Stat
	for stat := range statsChan {
		stats = append(stats, stat)
	}

	var successCount, failCount int
	var totalTime time.Duration
	var minTime, maxTime time.Duration

	for _, stat := range stats {
		if stat.Success {
			successCount++
			totalTime += stat.Duration
			if minTime == 0 || stat.Duration < minTime {
				minTime = stat.Duration
			}
			if stat.Duration > maxTime {
				maxTime = stat.Duration
			}
		} else {
			failCount++
		}
	}

	if successCount > 0 {
		avg := totalTime / time.Duration(successCount)
		jsonLog("summary", fmt.Sprintf("Success: %d, Failed: %d", successCount, failCount), "", 0)
		jsonLog("summary", fmt.Sprintf("Average: %s, Min: %s, Max: %s", avg, minTime, maxTime), "", 0)
	} else {
		jsonLog("summary", fmt.Sprintf("All %d runs failed", failCount), "", 0)
	}
}
