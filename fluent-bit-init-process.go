package main

import (
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/arn"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/sirupsen/logrus"
)

// static paths
const (
	s3FileFolderPath           = "fluent-bit-init-s3-files"
	mainConfigFilePath         = "fluent-bit-init.conf"
	originalMainConfigFilePath = "/fluent-bit/etc/fluent-bit.conf"
	invokerFilePath            = "fluent-bit-invoker.sh"
)

var (
	// default Fluent Bit command
	fluentBitCommand = "exec /fluent-bit/bin/fluent-bit -e /fluent-bit/firehose.so -e /fluent-bit/cloudwatch.so -e /fluent-bit/kinesis.so"

	// global s3 client and flag
	s3Client      *s3.S3
	exists3Client bool = false

	// global ecs metadata region
	metadataReigon string = ""
)

// HTTPClient interface
type HTTPClient interface {
	Get(url string) (*http.Response, error)
}

// S3Downloader interface
type S3Downloader interface {
	Download(w io.WriterAt, input *s3.GetObjectInput, options ...func(*s3manager.Downloader)) (int64, error)
}

type ECSTaskMetadata struct {
	AWS_REGION          string `json:"AWSRegion"`      // AWS region
	ECS_CLUSTER         string `json:"Cluster"`        // Cluster name
	ECS_TASK_ARN        string `json:"TaskARN"`        // Task ARN
	ECS_TASK_ID         string `json:"TaskID"`         // Task Id
	ECS_FAMILY          string `json:"Family"`         // Task family
	ECS_LAUNCH_TYPE     string `json:"LaunchType"`     // Task launch type, will be an empty string if container agent is under version 1.45.0
	ECS_REVISION        string `json:"Revision"`       // Revision number
	ECS_TASK_DEFINITION string `json:"TaskDefinition"` // TaskDefinition = "family:revision"
}

// create the HTTP Client
func createHTTPClient() HTTPClient {
	client := &http.Client{}
	return client
}

// get ECS Task Metadata via endpoint V4
func getECSTaskMetadata(httpClient HTTPClient) ECSTaskMetadata {

	var metadata ECSTaskMetadata

	ecsTaskMetadataEndpointV4 := os.Getenv("ECS_CONTAINER_METADATA_URI_V4")
	if ecsTaskMetadataEndpointV4 == "" {
		logrus.Warnln("[FluentBit Init Process] Unable to get ECS Metadata")
		return metadata
	}

	res, err := httpClient.Get(ecsTaskMetadataEndpointV4 + "/task")
	if err != nil {
		logrus.Warnln(err)
		logrus.Warnln("[FluentBit Init Process] Failed to get ECS Metadata via HTTP Get")
	}

	response, err := ioutil.ReadAll(res.Body)
	if err != nil {
		logrus.Warnln(err)
		logrus.Warnln("[FluentBit Init Process] Failed to read HTTP response")
	}
	res.Body.Close()

	err = json.Unmarshal(response, &metadata)
	if err != nil {
		logrus.Warnln(err)
		logrus.Warnln("[FluentBit Init Process] Failed to unmarshal ECS metadata")
	}

	arn, err := arn.Parse(metadata.ECS_TASK_ARN)
	if err != nil {
		logrus.Warnln(err)
		logrus.Warnln("[FluentBit Init Process] Failed to parse ECS TaskARN")
	}

	resourceID := strings.Split(arn.Resource, "/")
	taskID := resourceID[len(resourceID)-1]
	metadata.ECS_TASK_ID = taskID
	metadata.AWS_REGION = arn.Region
	metadata.ECS_TASK_DEFINITION = metadata.ECS_FAMILY + ":" + metadata.ECS_REVISION

	return metadata
}

// set ECS Task Metadata as environment variables in the fluent-bit-invoker.sh
func setECSTaskMetadata(metadata ECSTaskMetadata, filePath string) {

	t := reflect.TypeOf(metadata)
	v := reflect.ValueOf(metadata)

	invokerFile := openFile(filePath)
	defer invokerFile.Close()

	for i := 0; i < t.NumField(); i++ {
		if v.Field(i).Interface().(string) == "" {
			continue
		}
		writeContent := "export " + t.Field(i).Name + "=" + v.Field(i).Interface().(string) + "\n"
		_, err := invokerFile.WriteString(writeContent)
		if err != nil {
			logrus.Errorln(err)
			logrus.Fatalf("[FluentBit Init Process] Cannot write %s in the fluent-bit-invoker.sh\n", writeContent[:len(writeContent)-2])
		}
	}
}

// create Fluent Bit command to use new main config file
func createCommand(command *string, filePath string) {
	*command = *command + " -c /" + filePath
}

// create a folder to store S3 config files user specified
func createS3ConfigFileFolder(folderPath string) {
	os.Mkdir(folderPath, os.ModePerm)
}

// get our built in config file or files from s3
// process built-in config files directly
// add S3 config files to folder "init_process_fluent-bit_s3_config_files"
func getAllConfigFiles() {

	// get all env vars
	envs := os.Environ()
	// find all env vars match specified prefix
	for _, env := range envs {
		var envKey string
		var envValue string
		env_kv := strings.SplitN(env, "=", 2)
		if len(env_kv) != 2 {
			logrus.Fatalf("[FluentBit Init Process] Unrecognizable environment variables: %s\n", env)
		}

		envKey = string(env_kv[0])
		envValue = string(env_kv[1])

		matched_s3, _ := regexp.MatchString("aws_fluent_bit_s3_", envKey)
		matched_file, _ := regexp.MatchString("aws_fluent_bit_file_", envKey)
		// if this env var's value is an arn
		if matched_s3 {
			getS3ConfigFile(envValue)
		}
		// if this env var's value is a path of our built-in config file
		if matched_file {
			processConfigFile(envValue)
		}
	}
}

// process config files
func processConfigFile(path string) {

	content_b, err := ioutil.ReadFile(path)
	if err != nil {
		logrus.Errorln(err)
		logrus.Fatalf("[FluentBit Init Process] Cannot open file: %s\n", path)
	}
	content := string(content_b)
	if strings.Contains(content, "[PARSER]") {
		// this is a parser config file, change command
		updateCommand(path)
	} else {
		// this is not a parser config file. @INCLUDE
		writeInclude(path, mainConfigFilePath)
	}
}

// download S3 config file to S3 config file folder
func getS3ConfigFile(arn string) {

	// Preparation for downloading S3 config files
	if !exists3Client {
		createS3Client()
	}

	// e.g. "arn:aws:s3:::ygloa-bucket/s3_parser.conf"
	arnBucketFile := arn[13:]
	bucketAndFile := strings.SplitN(arnBucketFile, "/", 2)
	if len(bucketAndFile) != 2 {
		logrus.Fatalf("[FluentBit Init Process] Unrecognizable arn: %s\n", arn)
	}

	bucketName := bucketAndFile[0]
	s3FilePath := bucketAndFile[1]

	// get bucket region
	input := &s3.GetBucketLocationInput{
		Bucket: aws.String(bucketName),
	}

	output, err := s3Client.GetBucketLocation(input)
	if err != nil {
		logrus.Errorln(err)
		logrus.Fatalf("[FluentBit Init Process] Cannot get bucket region of %s + %s", bucketName, s3FilePath)
	}

	bucketRegion := aws.StringValue(output.LocationConstraint)
	// Buckets in Region us-east-1 have a LocationConstraint of null.
	// https://docs.aws.amazon.com/sdk-for-go/api/service/s3/#GetBucketLocationOutput
	if bucketRegion == "" {
		bucketRegion = "us-east-1"
	}

	// create downloader
	s3Downloader := createS3Downloader(bucketRegion)

	// download file from S3 and store
	downloadS3ConfigFile(s3Downloader, s3FilePath, bucketName)

}

// create a S3 client as the global S3 client for reuse
func createS3Client() {

	region := "us-east-1"
	if metadataReigon != "" {
		region = metadataReigon
	}
	s3Client = s3.New(session.Must(session.NewSession(&aws.Config{
		// if not specify region here, missingregion error will raise when get bucket location
		Region: aws.String(region),
	})))

	exists3Client = true
}

// create a S3 Downloader
func createS3Downloader(bucketRegion string) S3Downloader {
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(bucketRegion)},
	)
	if err != nil {
		logrus.Errorln(err)
		logrus.Fatalln("[FluentBit Init Process] Cannot creat a new session")
	}

	// need to specify session region!
	s3Downloader := s3manager.NewDownloader(sess)
	return s3Downloader
}

// download file from S3 and store
func downloadS3ConfigFile(s3Downloader S3Downloader, s3FilePath, bucketName string) {

	s3FileName := strings.SplitN(s3FilePath, "/", -1)
	fileFromS3 := createFile(s3FileFolderPath+"/"+s3FileName[len(s3FileName)-1], false)
	defer fileFromS3.Close()

	_, err := s3Downloader.Download(fileFromS3,
		&s3.GetObjectInput{
			Bucket: aws.String(bucketName),
			Key:    aws.String(s3FilePath),
		})
	if err != nil {
		logrus.Errorln(err)
		logrus.Fatalf("[FluentBit Init Process] Cannot download %s from s3\n", s3FileName)
	}
}

// process S3 config files user specified
func processS3ConfigFiles(folderPath string) {

	fileInfos, err := ioutil.ReadDir(folderPath)
	if err != nil {
		logrus.Errorln(err)
		logrus.Fatalf("[FluentBit Init Process] Unable to read config files in %s folder\n", folderPath)
	}

	for _, file := range fileInfos {
		filePath := "/" + folderPath + "/" + file.Name()
		processConfigFile(filePath)
	}
}

// use @INCLUDE to add config files to the main config file
func writeInclude(configFilePath, mainConfigFilePath string) {

	mainConfigFile := openFile(mainConfigFilePath)
	defer mainConfigFile.Close()

	writeContent := "@INCLUDE " + configFilePath + "\n"
	_, err := mainConfigFile.WriteString(writeContent)
	if err != nil {
		logrus.Errorln(err)
		logrus.Fatalf("[FluentBit Init Process] Cannot write %s in main config file: %s\n", writeContent[:len(writeContent)-2], mainConfigFilePath)
	}
}

// change the cammand if needed.
// add modified command to the init_process_entrypoint.sh
func updateCommand(parserFilePath string) {
	fluentBitCommand = fluentBitCommand + " -R " + parserFilePath
	logrus.Infoln("[FluentBit Init Process] Command is change to -> " + fluentBitCommand)
}

// change the fluent-bit-invoker.sh
// which will declare ECS Task Metadata as environment variables
// and finally invoke Fluent Bit
func modifyInvokerFile(filePath string) {

	invokerFile := openFile(filePath)
	defer invokerFile.Close()

	_, err := invokerFile.WriteString(fluentBitCommand)
	if err != nil {
		logrus.Errorln(err)
		logrus.Fatalf("[FluentBit Init Process] Cannot write %s in fluent-bit-invoker.sh\n", fluentBitCommand)
	}
}

// create a file, when flag is true, the file will be closed automatically after creation
func createFile(filePath string, AutoClose bool) *os.File {

	file, err := os.Create(filePath)
	if err != nil {
		logrus.Errorln(err)
		logrus.Fatalf("[FluentBit Init Process] Cannot create the file: %s\n", filePath)
	}
	if AutoClose {
		defer file.Close()
	}

	return file
}

func openFile(filePath string) *os.File {

	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_WRONLY, 0777)
	if err != nil {
		logrus.Errorln(err)
		logrus.Fatalf("[FluentBit Init Process] Unable to read %s\n", filePath)
	}
	return file
}

func main() {

	// create the fluent-bit-invoker.sh
	// which will declare ECS Task Metadata as environment variables
	// and finally invoke Fluent Bit
	createFile(invokerFilePath, true)

	// get ECS Task Metadata and set the region for S3 client
	httpClient := createHTTPClient()
	metadata := getECSTaskMetadata(httpClient)
	metadataReigon = reflect.ValueOf(metadata).Field(0).Interface().(string)

	// set ECS Task Metada as env vars in the fluent-bit-invoker.sh
	setECSTaskMetadata(metadata, invokerFilePath)

	// create main config file which will be used invoke Fluent Bit
	createFile(mainConfigFilePath, true)

	// add @INCLUDE in main config file to include all customer specified config files and original main config file
	writeInclude(originalMainConfigFilePath, mainConfigFilePath)

	// create Fluent Bit command to use new main config file
	createCommand(&fluentBitCommand, mainConfigFilePath)

	// create a S3 config files folder
	createS3ConfigFileFolder(s3FileFolderPath)

	// get our built in config file or files from s3
	// process built-in config files directly
	// add S3 config files to folder "init_process_fluent-bit_s3_config_files"
	getAllConfigFiles()

	// process all config files in config file folder, add @INCLUDE in main config file and change command
	processS3ConfigFiles(s3FileFolderPath)

	// modify fluent-bit-invoker.sh, invoke fluent bit
	// this function will be called at the end
	// any error appear above will cause exit this process, will not write Fluent Bit command in the invoker.sh so Fluent Bit will not be invoked
	modifyInvokerFile(invokerFilePath)
}
