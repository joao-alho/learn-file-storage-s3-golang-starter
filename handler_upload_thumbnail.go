package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	const maxMemory = 10 << 20
	if err := r.ParseMultipartForm(maxMemory); err != nil {
		respondWithError(w, http.StatusBadRequest, "something went wrong", err)
		return
	}
	thumbnailFile, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer thumbnailFile.Close()
	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if mediaType != "image/jpeg" && mediaType != "image/png" {
		respondWithError(w, http.StatusBadRequest, "invalid media type", nil)
		return
	}
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "video not found", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "unauthorized", nil)
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
	filePath := fmt.Sprintf("%s/%s.%s", cfg.assetsRoot, encodedRandom, fileExtension)
	file, err := os.Create(filePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error creating file", err)
		return
	}
	defer file.Close()
	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)
	_, err = io.Copy(file, thumbnailFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "failed to copy file contents", err)
		return
	}
	thumbnailUrl := fmt.Sprintf("http://localhost:%s/assets/%s.%s", cfg.port, encodedRandom, fileExtension)
	video.ThumbnailURL = &thumbnailUrl
	if err := cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "something went wrong", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}
