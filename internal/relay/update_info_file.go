package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/update"
)

const maxUpdateInfoFileBytes = 64 << 10

type updateInfoFileState struct {
	initialized bool
	exists      bool
	disabled    bool
	size        int64
	modTime     time.Time
	lastError   string
}

func (s *Server) rememberUpdateInfoFileMetadata(st os.FileInfo) {
	s.updateInfoFileMu.Lock()
	s.updateInfoFileState.initialized = true
	s.updateInfoFileState.exists = true
	s.updateInfoFileState.size = st.Size()
	s.updateInfoFileState.modTime = st.ModTime()
	s.updateInfoFileMu.Unlock()
}

// updateInfoLoop watches only the small announcement file. It performs a cheap
// stat at a low frequency and parses JSON only when file metadata changes.
func (s *Server) updateInfoLoop(ctx context.Context) {
	if s.updateInfoFile == "" {
		return
	}
	ticker := time.NewTicker(s.updateInfoPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.reloadUpdateInfoFile(); err != nil {
				s.logUpdateInfoFileError(err)
			}
		}
	}
}

func (s *Server) logUpdateInfoFileError(err error) {
	s.updateInfoFileMu.Lock()
	defer s.updateInfoFileMu.Unlock()
	msg := err.Error()
	if s.updateInfoFileState.lastError == msg {
		return
	}
	s.updateInfoFileState.lastError = msg
	log.Printf("update info file reload failed: %v (keeping previous announcement)", err)
}

func (s *Server) reloadUpdateInfoFile() error {
	info, changed, err := s.readUpdateInfoFile()
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	s.updateInfoFileMu.Lock()
	disabled := s.updateInfoFileState.disabled
	exists := s.updateInfoFileState.exists
	s.updateInfoFileMu.Unlock()
	if !exists {
		info = s.updateInfoBase
	} else if !disabled {
		info = mergeUpdateInfo(s.updateInfoBase, info)
	}
	s.updateInfoValue.Store(info)
	s.updateInfoFileMu.Lock()
	s.updateInfoFileState.lastError = ""
	s.updateInfoFileMu.Unlock()
	log.Printf("update info reloaded: latest client version %q", info.LatestVersion)
	return nil
}

func (s *Server) readUpdateInfoFile() (proto.UpdateInfo, bool, error) {
	st, err := os.Stat(s.updateInfoFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.updateInfoFileMu.Lock()
			changed := !s.updateInfoFileState.initialized || s.updateInfoFileState.exists
			s.updateInfoFileState.initialized = true
			s.updateInfoFileState.exists = false
			s.updateInfoFileState.disabled = false
			s.updateInfoFileMu.Unlock()
			return proto.UpdateInfo{}, changed, nil
		}
		return proto.UpdateInfo{}, false, fmt.Errorf("stat %s: %w", s.updateInfoFile, err)
	}
	if st.IsDir() {
		return proto.UpdateInfo{}, false, fmt.Errorf("update info path is a directory: %s", s.updateInfoFile)
	}
	s.updateInfoFileMu.Lock()
	unchanged := s.updateInfoFileState.initialized && s.updateInfoFileState.exists &&
		s.updateInfoFileState.size == st.Size() && s.updateInfoFileState.modTime.Equal(st.ModTime())
	s.updateInfoFileMu.Unlock()
	if unchanged {
		return proto.UpdateInfo{}, false, nil
	}
	file, err := os.Open(s.updateInfoFile)
	if err != nil {
		return proto.UpdateInfo{}, false, fmt.Errorf("open %s: %w", s.updateInfoFile, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxUpdateInfoFileBytes+1))
	if err != nil {
		return proto.UpdateInfo{}, false, fmt.Errorf("read %s: %w", s.updateInfoFile, err)
	}
	if len(data) > maxUpdateInfoFileBytes {
		s.rememberUpdateInfoFileMetadata(st)
		return proto.UpdateInfo{}, false, fmt.Errorf("update info file exceeds %d bytes", maxUpdateInfoFileBytes)
	}
	info, err := parseUpdateInfoFile(data)
	if err != nil {
		s.rememberUpdateInfoFileMetadata(st)
		return proto.UpdateInfo{}, false, fmt.Errorf("parse %s: %w", s.updateInfoFile, err)
	}
	s.rememberUpdateInfoFileMetadata(st)
	var envelope struct {
		LatestVersion string `json:"latest_version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return proto.UpdateInfo{}, false, fmt.Errorf("parse %s: %w", s.updateInfoFile, err)
	}
	s.updateInfoFileMu.Lock()
	s.updateInfoFileState.disabled = strings.EqualFold(strings.TrimSpace(envelope.LatestVersion), "none")
	s.updateInfoFileMu.Unlock()
	return info, true, nil
}

func parseUpdateInfoFile(data []byte) (proto.UpdateInfo, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var info proto.UpdateInfo
	if err := dec.Decode(&info); err != nil {
		return proto.UpdateInfo{}, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return proto.UpdateInfo{}, errors.New("multiple JSON values")
		}
		return proto.UpdateInfo{}, err
	}
	info.LatestVersion = strings.TrimSpace(info.LatestVersion)
	if strings.EqualFold(info.LatestVersion, "none") {
		return proto.UpdateInfo{}, nil
	}
	for name, value := range map[string]string{"download_url": info.DownloadURL, "release_page": info.ReleasePage} {
		if value != "" && !update.SafeURL(value) {
			return proto.UpdateInfo{}, fmt.Errorf("%s must be an https URL without spaces or control characters", name)
		}
	}
	if info.LatestVersion == "" {
		// An omitted version means "keep the startup version". Keep the URL
		// overrides so operators can change the link without repeating the version.
		return info, nil
	}
	if !update.IsRelease(info.LatestVersion) {
		return proto.UpdateInfo{}, fmt.Errorf("latest_version %q must be a release version or none", info.LatestVersion)
	}
	return info, nil
}

func mergeUpdateInfo(base, override proto.UpdateInfo) proto.UpdateInfo {
	if override.LatestVersion == "" {
		override.LatestVersion = base.LatestVersion
	}
	if override.DownloadURL == "" {
		override.DownloadURL = base.DownloadURL
	}
	if override.ReleasePage == "" {
		override.ReleasePage = base.ReleasePage
	}
	override.LatestVersion = update.NormalizeDefaultURLVersion(override.LatestVersion, override.DownloadURL, override.ReleasePage)
	return override
}

func normalizeUpdateInfo(info proto.UpdateInfo) proto.UpdateInfo {
	info.LatestVersion = update.NormalizeDefaultURLVersion(info.LatestVersion, info.DownloadURL, info.ReleasePage)
	return info
}
