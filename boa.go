package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"

	rj "github.com/amosmzhang/rapidjson" // Faster JSON helper
	"github.com/streadway/amqp"          // RabbitMQ
	"github.com/urfave/cli"              // CLI helper
	"gopkg.in/amz.v3/aws"                // AWS library
	"gopkg.in/amz.v3/s3"                 // S3 library
	"gopkg.in/mgo.v2"                    // Mongo
	"gopkg.in/mgo.v2/bson"               // Mongo BSON
)

const (
	HUNT_BUFSIZE      = 1000
	CONSTRICT_BUFSIZE = 1000
	DIGEST_BUFSIZE    = 1000
)

var (
	// pseudo-consts set at runtime
	RABBIT_DIAL   string
	MONGO_DIAL    string
	DB_NAME       string
	DB_COLLECTION string
	S3_BUCKET     string
	MP3_COMMENT   string

	awsAuth = aws.Auth{
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
	awsRegion       = aws.USWest2 // Oregon
	bucket          *s3.Bucket
	huntChan        chan Message
	constrictChan   chan Message
	digestChan      chan Message
	audioCollection *mgo.Collection
)

// Message is passed from channel to channel to let boa work its magic
type Message struct {
	ID         string
	Filename   string
	StreamOnly bool
	Delivery   amqp.Delivery
}

// Returns the maximum reasonable parallelism to use
func maxParallelism() int {
	maxProcs := runtime.GOMAXPROCS(0)
	numCPU := runtime.NumCPU()
	if maxProcs < numCPU {
		return maxProcs
	}
	return numCPU
}

// Logs error and panics if we experience a fatal Rabbit issue
func rabbitError(err error, msg string) {
	if err != nil {
		log.Fatal("!!! FATAL " + msg + ": " + err.Error())
	}
}

// Compresses to either MP3 128 or MP3 320 using lame
func CompressLame(sourceName string, outputPath string, stream bool) error {
	var cmd *exec.Cmd
	if stream { // MP3 128
		cmd = exec.Command("lame", "-q", "0", "-b", "128", "--cbr", "--tc", MP3_COMMENT, "files/original-upload/"+sourceName, "files/"+outputPath, "--silent")
	} else { // MP3 320
		cmd = exec.Command("lame", "-q", "0", "-b", "320", "--cbr", "--tc", MP3_COMMENT, "files/original-upload/"+sourceName, "files/"+outputPath, "--silent")
	}

	if err := cmd.Start(); err != nil {
		return err
	}
	if err := cmd.Wait(); err != nil {
		return err
	}

	// Ensure file exists after successful conversion
	if _, err := os.Stat("files/" + outputPath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("MP3 Compression failed: " + outputPath)
		}
	}
	return nil
}

func main() {
	app := cli.NewApp()
	app.Version = "0.2.2"
	app.Name = "boa"
	app.Usage = "Friendly neighborhood snake-boy trained to compress audio uploaded to S3"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "rabbitDial, rd",
			Value:  "amqp://guest:guest@localhost:5672/",
			Usage:  "RabbitMQ connection string",
			EnvVar: "RABBIT_DIAL",
		},
		cli.StringFlag{
			Name:   "mongoDial, md",
			Value:  "mongodb://localhost/",
			Usage:  "MongoDB connection string",
			EnvVar: "MONGO_DIAL",
		},
		cli.StringFlag{
			Name:   "dbName, db",
			Value:  "boa-dev",
			Usage:  "Name of database to connect to",
			EnvVar: "DB_NAME",
		},
		cli.StringFlag{
			Name:   "dbCollection, c",
			Value:  "audio",
			Usage:  "Name of database collection to use",
			EnvVar: "DB_COLLECTION",
		},
		cli.StringFlag{
			Name:   "s3Bucket, s",
			Value:  "boa-audio",
			Usage:  "S3 bucket which stores all uploaded audio files",
			EnvVar: "S3_BUCKET",
		},
		cli.StringFlag{
			Name:   "mp3Coment, mc",
			Value:  "Constricted by boa",
			Usage:  "Comment to include in MP3 metadata",
			EnvVar: "MP3_COMMENT",
		},
	}

	app.Commands = []cli.Command{
		{
			Name:  "run",
			Usage: "./boa [options] run",
			Action: func(c *cli.Context) {
				RABBIT_DIAL = c.GlobalString("rabbitDial")
				MONGO_DIAL = c.GlobalString("mongoDial")
				DB_NAME = c.GlobalString("dbName")
				DB_COLLECTION = c.GlobalString("dbCollection")
				S3_BUCKET = c.GlobalString("s3Bucket")
				MP3_COMMENT = c.GlobalString("mp3Comment")
				start()
			},
		},
	}

	app.Run(os.Args)
}

func start() {
	// Connect to Mongo
	sess, err := mgo.Dial(MONGO_DIAL + DB_NAME)
	if err != nil {
		log.Fatal("!!! FATAL Could not connect to Mongo")
	}
	defer sess.Close()
	sess.SetSafe(&mgo.Safe{})
	audioCollection = sess.DB(DB_NAME).C(DB_COLLECTION)

	// Connect to S3
	connection := s3.New(awsAuth, awsRegion)
	bucket, err = connection.Bucket(S3_BUCKET)
	if err != nil {
		log.Fatal("!!! FATAL Could not connect to S3 bucket "+S3_BUCKET, err)
	}

	huntChan = make(chan Message, HUNT_BUFSIZE)
	constrictChan = make(chan Message, CONSTRICT_BUFSIZE)
	digestChan = make(chan Message, DIGEST_BUFSIZE)
	defer close(huntChan)
	defer close(constrictChan)
	defer close(digestChan)

	// Create parellel goroutines for each task
	for i := 0; i < maxParallelism(); i++ {
		go Hunt()
		go Constrict()
		go Digest()
	}

	// Start stalking prey
	Stalk()
}

// Get messages from Rabbit and pass them to Hunt()
func Stalk() {
	// Setup and connect to Rabbit
	conn, err := amqp.Dial(RABBIT_DIAL)
	rabbitError(err, "Failed to connect to RabbitMQ")
	defer conn.Close()

	ch, err := conn.Channel()
	rabbitError(err, "Failed to open a channel")
	defer ch.Close()

	err = ch.ExchangeDeclare(
		"amq.topic", // name
		"topic",     // type
		true,        // durable
		false,       // auto-deleted
		false,       // internal
		false,       // no-wait
		nil,         // arguments
	)
	rabbitError(err, "Failed to declare an exchange")

	q, err := ch.QueueDeclare(
		"boa-stalk", // name
		true,        // durable
		false,       // delete when used
		false,       // exclusive
		false,       // no-wait
		nil,         // arguments
	)
	rabbitError(err, "Failed to declare a queue")

	msgs, err := ch.Consume(
		q.Name, // queue
		"",     // consumer
		false,  // auto-ack
		false,  // exclusive
		false,  // no-local
		false,  // no-wait
		nil,    // args
	)
	rabbitError(err, "Failed to register a consumer")

	err = ch.QueueBind(
		q.Name,      // queue name
		"audio",     // routing key
		"amq.topic", // exchange
		false,
		nil,
	)
	rabbitError(err, "Failed to bind a queue")

	// Wait on new messages to come in off queue
	for d := range msgs {
		fmt.Println(string(d.Body))
		msgJson, err := rj.NewParsedJson(d.Body)
		if err != nil {
			log.Printf("!!! ERROR Cannot parse message: " + err.Error())
			d.Ack(false)
		}
		msgJsonCt := msgJson.GetContainer()
		path, _ := msgJsonCt.GetPathContainerOrNil("originalUploadPath").GetString()
		if path == "" {
			log.Printf("!!! ERROR Cannot parse message: originalUploadPath empty")
			d.Ack(false)
		}
		id, _ := msgJsonCt.GetPathContainerOrNil("id").GetString()
		if id == "" {
			log.Printf("!!! ERROR Cannot parse message: id empty")
			d.Ack(false)
		}
		streamOnly, _ := msgJsonCt.GetPathContainerOrNil("streamOnly").GetBool()
		path = strings.Replace(path, "\"", "", -1)
		path = strings.Replace(path, "\\u003c", "<", -1)
		path = strings.Replace(path, "\\u003e", "<", -1)
		path = strings.Replace(path, "\\u0026", "&", -1)
		fileSplit := strings.Split(path, "/")
		os.Mkdir("files/original-upload/"+fileSplit[1], os.ModePerm)
		os.Mkdir("files/converted-320/"+fileSplit[1], os.ModePerm)
		os.Mkdir("files/stream/"+fileSplit[1], os.ModePerm)
		huntChan <- Message{Delivery: d, ID: id, StreamOnly: streamOnly, Filename: fileSplit[1] + "/" + fileSplit[2]}
		msgJson.Free()
	}
}

// Download original uploads from AWS and pass to Constrict()
func Hunt() {
	for {
		msg, ok := <-huntChan
		if ok {
			filename := msg.Filename

			// Get file from S3 bucket
			downloadBytes, err := bucket.Get("original-upload/" + filename)
			if err != nil {
				log.Printf("!!! ERROR ", err, filename)
				msg.Delivery.Nack(false, true)
				continue
			}

			// Create file locally
			downloadFile, err := os.Create("files/original-upload/" + filename)
			defer downloadFile.Close()
			if err != nil {
				log.Printf("!!! ERROR ", err)
				msg.Delivery.Nack(false, true)
				continue
			}
			downloadBuffer := bufio.NewWriter(downloadFile)
			downloadBuffer.Write(downloadBytes)
			io.Copy(downloadBuffer, downloadFile)
			downloadBuffer.Flush()
			log.Printf("--- DOWNLOAD " + filename)
			constrictChan <- msg
		}
	}
}

// Compresses file and passes to Digest()
func Constrict() {
	for {
		msg, ok := <-constrictChan
		if ok {
			filename := msg.Filename

			// Use "file" to determine codec type
			cmdFile := exec.Command("file", "files/original-upload/"+filename)
			stdout, err := cmdFile.StdoutPipe()
			if err != nil {
				log.Printf("!!! ERROR ", err)
				msg.Delivery.Nack(false, true)
				continue
			}
			if err = cmdFile.Start(); err != nil {
				log.Printf("!!! ERROR ", err)
				msg.Delivery.Nack(false, true)
				continue
			}
			buf := new(bytes.Buffer)
			buf.ReadFrom(stdout)
			fileInfo := buf.String()
			if err = cmdFile.Wait(); err != nil {
				log.Printf("!!! ERROR ", err)
				msg.Delivery.Nack(false, true)
				continue
			}

			streamOnly := msg.StreamOnly
			if strings.Contains(fileInfo, "MPEG ADTS, layer III") {
				// Convert only for streaming if stream filesize is smaller than original filesize
				streamOnly = true
			} else if !strings.Contains(fileInfo, "WAVE audio") && !strings.Contains(fileInfo, "AIFF audio") {
				// Unsupported codec
				log.Printf("!!! ERROR Invalid codec detected: " + filename)
				msg.Delivery.Ack(false)
				// mongo invalid flag
				if err = os.Remove("files/original-upload/" + filename); err != nil {
					log.Printf("!!! ERROR Problem removing files/original-upload/" + filename)
				}
				continue
			}

			// Generate new file name - replace expension (if .wav or .aiff) or add .mp3
			var mp3File string
			if strings.HasSuffix(strings.ToLower(filename), ".wav") {
				mp3File = filename[:strings.LastIndex(strings.ToLower(filename), ".wav")] + ".mp3"
			} else if strings.HasSuffix(strings.ToLower(filename), ".aiff") {
				mp3File = filename[:strings.LastIndex(strings.ToLower(filename), ".aiff")] + ".mp3"
			} else if !strings.HasSuffix(strings.ToLower(filename), ".mp3") {
				mp3File = filename + ".mp3"
			} else {
				mp3File = filename
			}

			// Convert to MP3 V2 - stream
			if err := CompressLame(filename, "stream/"+mp3File, true); err != nil {
				log.Printf("!!! ERROR ", err)
			} else {
				log.Printf("--- COMPRESS original-upload/" + filename + " > stream/" + mp3File)
				// Only upload stream if smaller than originally uploaded mp3
				if streamOnly {
					streamSizeFile, _ := os.Open("files/stream/" + mp3File)
					originalSizeFile, _ := os.Open("files/original-upload/" + filename)
					defer streamSizeFile.Close()
					defer originalSizeFile.Close()
					streamSize, _ := streamSizeFile.Stat()
					originalSize, _ := originalSizeFile.Stat()
					if streamSize.Size() < originalSize.Size() {
						msg.Filename = "stream/" + mp3File
						digestChan <- msg
					} else {
						log.Printf("--- NO UPLOAD original upload smaller than stream")
						msg.Delivery.Ack(false)
						if err = os.Remove("files/stream/" + mp3File); err != nil {
							log.Printf("!!! ERROR Problem removing files/stream/" + mp3File)
						}
						// tell mongo its over
					}
				} else {
					msg.Filename = "stream/" + mp3File
					digestChan <- msg
				}
			}

			// Convert to MP3 320 if we started with lossless
			if !streamOnly {
				if err := CompressLame(filename, "converted-320/"+mp3File, false); err != nil {
					log.Printf("!!! ERROR ", err)
				} else {
					log.Printf("--- COMPRESS original-upload/" + filename + " > converted-320/" + mp3File)
					msg.Filename = "converted-320/" + mp3File
					digestChan <- msg
				}
			}

			// Remove uploaded file
			if err = os.Remove("files/original-upload/" + filename); err != nil {
				log.Printf("!!! ERROR Problem removing files/original-upload/" + filename)
			}
		}
	}
}

// Upload newly compressed file to AWS and delete local files
func Digest() {
	for {
		msg, ok := <-digestChan
		if ok {
			path := msg.Filename

			// Open file
			file, err := os.Open("files/" + path)
			defer file.Close()
			if err != nil {
				log.Printf("!!! ERROR " + err.Error())
				msg.Delivery.Nack(false, true)
				continue
			}

			// Get file size
			fileInfo, _ := file.Stat()
			var size int64 = fileInfo.Size()
			bytes := make([]byte, size)

			// Read file into buffer
			buffer := bufio.NewReader(file)
			_, err = buffer.Read(bytes)
			if err != nil {
				log.Printf("!!! ERROR " + err.Error())
				msg.Delivery.Nack(false, true)
				continue
			}

			// Upload to S3
			filetype := http.DetectContentType(bytes)
			err = bucket.Put(path, bytes, filetype, s3.PublicRead)
			if err != nil {
				log.Printf("!!! ERROR " + err.Error())
				msg.Delivery.Nack(false, true)
				continue
			}

			log.Printf("--- UPLOAD " + path)

			// Update newly uploaded path in mongo
			pathSplit := strings.Split(path, "/")
			url := bucket.URL(path)
			if len(url) > 0 {
				_, err = audioCollection.Find(bson.M{"_id": bson.ObjectIdHex(msg.ID)}).Apply(
					mgo.Change{
						Update: bson.M{"$set": bson.M{pathSplit[0]: url}},
						Upsert: true,
					}, nil)
				if err != nil {
					log.Fatal("!!! FATAL Cannot update " + msg.ID + " in Mongo:" + err.Error())
				}
			}

			// Remove uploaded file
			if err = os.Remove("files/" + path); err != nil {
				log.Printf("!!! ERROR Problem removing files/" + path)
			}

			msg.Delivery.Ack(false)
		}
	}
}
