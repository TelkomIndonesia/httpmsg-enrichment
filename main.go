package main

import (
	"log"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/gin-gonic/gin"
)

func main() {
	cfg, err := newConfig()
	if err != nil {
		log.Fatalf("error loading config: %v", err)
	}
	log.Print(cfg)

	s3Config := &aws.Config{
		Credentials:      credentials.NewStaticCredentials(cfg.S3.Credentials.KeyID, cfg.S3.Credentials.SecretKey, ""),
		Endpoint:         aws.String(cfg.S3.Endpoint),
		Region:           aws.String(cfg.S3.Region),
		S3ForcePathStyle: aws.Bool(cfg.S3.ForcePathStyle),
	}
	s3Client := s3.New(session.New(s3Config))

	erc, err := newEnricher()
	if err != nil {
		log.Fatalf("error initializing enricher: %v", err)
	}

	r := gin.Default()
	r.UseRawPath = true
	r.UnescapePathValues = true
	r.GET("/ecs/s3/:object_key", func(c *gin.Context) {
		getInput := &s3.GetObjectInput{
			Bucket: aws.String("logs"),
			Key:    aws.String(c.Param("object_key")),
		}
		resp, err := s3Client.GetObject(getInput)
		if err != nil {
			c.String(500, err.Error())
			return
		}
		defer resp.Body.Close()

		score, err := erc.EnrichRecord(resp.Body)
		if err != nil {
			c.String(500, err.Error())
			return
		}

		c.JSON(http.StatusOK, score)
	})

	r.GET("/ecs/files/:filename", func(c *gin.Context) {
		f, err := os.Open(c.Param("filename"))
		if err != nil {
			c.String(500, err.Error())
			return
		}
		defer f.Close()

		score, err := erc.EnrichRecord(f)
		if err != nil {
			c.String(500, err.Error())
			return
		}

		c.JSON(http.StatusOK, score)
	})

	r.Run()
}
