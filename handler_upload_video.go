package main

import (
	"bytes"
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

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
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
		respondWithError(w, http.StatusInternalServerError, "something went wrong", err)
		return
	}
	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "something went wrong", err)
		return
	}
	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &s3FileKey,
		Body:        processedFile,
		ContentType: &mediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "something went wrong", err)
		return
	}

	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, s3FileKey)
	fmt.Println(videoURL)
	video.VideoURL = &videoURL
	if err := cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "something went wrong", err)
		return
	}
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
