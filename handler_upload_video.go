package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const uploadLimit = 1 << 30
	http.MaxBytesReader(w, r.Body, uploadLimit)
	videoID := r.PathValue("videoID")
	videoUUID, err := uuid.Parse(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "invalid video id", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "unauthorized", err)
		return
	}

	user, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "unauthorized", err)
		return
	}

	video, err := cfg.db.GetVideo(videoUUID)
	if err != nil {
		respondWithError(w, http.StatusNoContent, "video not found", err)
		return
	}
	if video.UserID != user {
		respondWithError(w, http.StatusUnauthorized, "unaythorized", nil)
		return
	}

	file, fileHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "something went wrong", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(fileHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "somewhitn went wrong", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "invalid Content-Type", nil)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "something went wrong", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()
	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "something went wrong", err)
		return
	}

	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "something went wrong", err)
		return
	}

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "something went wrong", err)
		return
	}

	fileExtension := strings.Split(mediaType, "/")[1]
	val := make([]byte, 32)
	_, err = rand.Read(val)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "something went wrong", err)
		return
	}
	encodedRandom := base64.RawURLEncoding.EncodeToString(val)
	var s3FileKey string
	switch aspectRatio {
	case "16:9":
		s3FileKey = fmt.Sprintf("landscape/%s.%s", encodedRandom, fileExtension)
	case "9:16":
		s3FileKey = fmt.Sprintf("portrait/%s.%s", encodedRandom, fileExtension)
	case "other":
		s3FileKey = fmt.Sprintf("other/%s.%s", encodedRandom, fileExtension)
	}
	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error processing video", err)
		return
	}
	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not open processed file", err)
		return
	}
	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(s3FileKey),
		Body:        processedFile,
		ContentType: aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error upload file to S3", err)
		return
	}

	videoURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, s3FileKey)
	video.VideoURL = &videoURL
	if err := cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't update video", err)
		return
	}
	signedVideo, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't generate presinged url", err)
		return
	}
	respondWithJSON(w, http.StatusOK, signedVideo)
}

func getVideoAspectRatio(filePath string) (string, error) {
	var stdout bytes.Buffer
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return stdout.String(), err
	}
	type ffprobe struct {
		Streams []struct {
			CodecName          string `json:"codec_name"`
			CodecType          string `json:"codec_type"`
			DisplayAspectRatio string `json:"display_aspect_ratio"`
			Width              int    `json:"width"`
			Height             int    `json:"height"`
		} `json:"streams"`
	}
	var cmdOut ffprobe
	if err := json.Unmarshal(stdout.Bytes(), &cmdOut); err != nil {
		return "", err
	}
	var aspect_ratio string
	for _, s := range cmdOut.Streams {
		if s.CodecType == "video" {
			aspect_ratio = s.DisplayAspectRatio
			break
		}
	}
	return aspect_ratio, nil
}

func processVideoForFastStart(filePath string) (string, error) {
	outputFilePath := fmt.Sprintf("%s.processing", filePath)
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilePath)
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return outputFilePath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	r, err := presignClient.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: &bucket, Key: &key,
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned url: %v", err)
	}
	return r.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}
	parts := strings.Split(*video.VideoURL, ",")
	if len(parts) < 2 {
		return video, nil
	}
	bucket := parts[0]
	key := parts[1]
	presignedURL, err := generatePresignedURL(cfg.s3Client, bucket, key, 5*time.Minute)
	if err != nil {
		return video, err
	}
	video.VideoURL = &presignedURL
	return video, nil
}
