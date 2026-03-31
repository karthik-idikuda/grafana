// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

// Code from https://github.com/gogits/gogs/blob/v0.7.0/modules/avatar/avatar.go

package avatar

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"

	"github.com/grafana/grafana/pkg/infra/log"
	contextmodel "github.com/grafana/grafana/pkg/services/contexthandler/model"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/web"
)

// Avatar represents the avatar object.
type Avatar struct {
	hash     string
	data     []byte
	notFound bool
	isCustom bool
}

func (a *Avatar) Encode(wr io.Writer) error {
	_, err := wr.Write(a.data)
	return err
}

func (a *Avatar) GetIsCustom() bool {
	return a.isCustom
}

type AvatarCacheServer struct {
	cfg             *setting.Cfg
	notFound        *Avatar
	cache           *lru.LRU[string, *Avatar]
	logger          log.Logger
	gravatarBaseURL string
	client          *http.Client
}

var looksLikeMD5 = regexp.MustCompile("^[a-fA-F0-9]{32}$")

func (a *AvatarCacheServer) Handler(ctx *contextmodel.ReqContext) {
	hash := web.Params(ctx.Req)[":hash"]

	if len(hash) != 32 || !looksLikeMD5.MatchString(hash) {
		ctx.JsonApiErr(404, "Avatar not found", nil)
		return
	}

	avatar := a.GetAvatarForHash(ctx.Req.Context(), a.cfg, hash)

	ctx.Resp.Header().Set("Content-Type", "image/jpeg")

	if !a.cfg.EnableGzip {
		ctx.Resp.Header().Set("Content-Length", strconv.Itoa(len(avatar.data)))
	}

	ctx.Resp.Header().Set("Cache-Control", "private, max-age=3600")

	if err := avatar.Encode(ctx.Resp); err != nil {
		ctx.Logger.Warn("avatar encode error:", "err", err)
		ctx.Resp.WriteHeader(http.StatusInternalServerError)
	}
}

func (a *AvatarCacheServer) GetAvatarForHash(ctx context.Context, cfg *setting.Cfg, hash string) *Avatar {
	if cfg.DisableGravatar {
		a.logger.Warn("'GetGravatarForHash' called despite gravatars being disabled; returning default profile image")
		return a.notFound
	}
	return a.getAvatarForHash(ctx, hash)
}

func (a *AvatarCacheServer) getAvatarForHash(ctx context.Context, hash string) *Avatar {
	// This also handles expiration, so if the cache is hit but expired, it will return exists=false with the data.
	avatar, exists := a.cache.Get(hash)
	if exists && avatar != nil {
		// Return the singleton so we dont need to store all the same bytes in cache.
		if avatar.notFound {
			return a.notFound
		}

		return avatar
	}

	avatar, err := a.getAvatarRemote(ctx, hash)
	if err != nil {
		// For any temporary or permanent errors, fall back to the not found image, this flow shouldn't break anything.
		a.logger.Debug("get avatar", "err", err)
		avatar = a.notFound
	}

	if evicted := a.cache.Add(hash, avatar); evicted {
		a.logger.Debug("add avatar to cache", "hash", hash, "evicted", evicted)
	}

	return avatar
}

func (a *AvatarCacheServer) getAvatarRemote(ctx context.Context, hash string) (*Avatar, error) {
	// Parameters needed to fetch Gravatar with a retro fallback
	var gravatarReqParams = url.Values{
		"d":    {"retro"},
		"size": {"200"},
		"r":    {"pg"},
	}.Encode()

	fullURL := a.gravatarBaseURL + "/" + hash + "?" + gravatarReqParams

	avatarData, err := a.performGet(ctx, fullURL)
	if err != nil {
		return nil, fmt.Errorf("failed to get avatar: %w", err)
	}

	isCustom := a.isAvatarCustomRemote(ctx, hash)

	return &Avatar{
		hash:     hash,
		data:     avatarData,
		notFound: false,
		isCustom: isCustom,
	}, nil
}

func (a *AvatarCacheServer) isAvatarCustomRemote(ctx context.Context, hash string) bool {
	// Parameters needed to see if a Gravatar is custom
	var gravatarReqParams = url.Values{
		"d": {"404"},
	}.Encode()

	fullURL := a.gravatarBaseURL + "/" + hash + "?" + gravatarReqParams

	_, err := a.performGet(ctx, fullURL)
	return err == nil
}

func ProvideAvatarCacheServer(cfg *setting.Cfg) *AvatarCacheServer {
	logger := log.New("avatar")
	return &AvatarCacheServer{
		cfg:             cfg,
		notFound:        newNotFound(cfg, logger),
		cache:           lru.NewLRU[string, *Avatar](2000, nil, time.Hour),
		logger:          logger,
		gravatarBaseURL: strings.TrimSuffix(cfg.GravatarURL, "/"),
		client: &http.Client{
			Timeout:   time.Second * 2,
			Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
		},
	}
}

func newNotFound(cfg *setting.Cfg, logger log.Logger) *Avatar {
	avatar := &Avatar{
		notFound: true,
	}

	// load user_profile png into buffer
	// It's safe to ignore gosec warning G304 since the variable part of the file path comes from a configuration
	// variable.
	// nolint:gosec
	path := filepath.Join(cfg.StaticRootPath, "img", "user_profile.png")

	// It's safe to ignore gosec warning G304 since the variable part of the file path comes from a configuration
	// variable.
	// nolint:gosec
	if data, err := os.ReadFile(path); err != nil {
		logger.Error("Failed to read user_profile.png", "path", path)
	} else {
		avatar.data = data
	}

	return avatar
}

func (a *AvatarCacheServer) performGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/jpeg,image/png,*/*;q=0.8")
	req.Header.Set("Accept-Encoding", "deflate,sdch")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 6.1; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/33.0.1750.154 Safari/537.36")

	a.logger.Debug("Fetching avatar url with parameters", "url", url)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gravatar unreachable: %w", err)
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			a.logger.Warn("Failed to close response body", "err", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status code: %d", resp.StatusCode)
	}

	var (
		data []byte
		rerr error
	)
	if resp.ContentLength > 0 {
		data = make([]byte, resp.ContentLength)
		_, rerr = io.ReadFull(resp.Body, data)
	} else {
		data, rerr = io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB
	}

	return data, rerr
}
