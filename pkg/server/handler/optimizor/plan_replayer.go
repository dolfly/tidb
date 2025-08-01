// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package optimizor

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gorilla/mux"
	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/domain"
	"github.com/pingcap/tidb/pkg/domain/infosync"
	"github.com/pingcap/tidb/pkg/infoschema"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/server/handler"
	"github.com/pingcap/tidb/pkg/statistics/handle"
	util2 "github.com/pingcap/tidb/pkg/statistics/util"
	"github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"github.com/pingcap/tidb/pkg/util/replayer"
	"go.uber.org/zap"
)

// PlanReplayerHandler is the handler for dumping plan replayer file.
type PlanReplayerHandler struct {
	is          infoschema.InfoSchema
	statsHandle *handle.Handle
	infoGetter  *infosync.InfoSyncer
	address     string
	statusPort  uint
}

// NewPlanReplayerHandler creates a new PlanReplayerHandler.
func NewPlanReplayerHandler(is infoschema.InfoSchema, statsHandle *handle.Handle, infoGetter *infosync.InfoSyncer, address string, statusPort uint) *PlanReplayerHandler {
	return &PlanReplayerHandler{
		is:          is,
		statsHandle: statsHandle,
		infoGetter:  infoGetter,
		address:     address,
		statusPort:  statusPort,
	}
}

// ServeHTTP handles request of dumping plan replayer file.
func (prh PlanReplayerHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	params := mux.Vars(req)
	name := params[handler.FileName]
	handler := downloadFileHandler{
		filePath:           filepath.Join(replayer.GetPlanReplayerDirName(), name),
		fileName:           name,
		infoGetter:         prh.infoGetter,
		address:            prh.address,
		statusPort:         prh.statusPort,
		urlPath:            fmt.Sprintf("plan_replayer/dump/%s", name),
		downloadedFilename: "plan_replayer",
		scheme:             util.InternalHTTPSchema(),
		statsHandle:        prh.statsHandle,
		is:                 prh.is,
	}
	handleDownloadFile(handler, w, req)
}

func handleDownloadFile(dfHandler downloadFileHandler, w http.ResponseWriter, req *http.Request) {
	params := mux.Vars(req)
	name := params[handler.FileName]
	path := dfHandler.filePath
	isForwarded := len(req.URL.Query().Get("forward")) > 0
	localAddr := net.JoinHostPort(dfHandler.address, strconv.Itoa(int(dfHandler.statusPort)))
	exist, err := isExists(path)
	if err != nil {
		handler.WriteError(w, err)
		return
	}
	if exist {
		//nolint: gosec
		file, err := os.Open(path)
		if err != nil {
			handler.WriteError(w, err)
			return
		}
		content, err := io.ReadAll(file)
		if err != nil {
			handler.WriteError(w, err)
			return
		}
		err = file.Close()
		if err != nil {
			handler.WriteError(w, err)
			return
		}
		if dfHandler.downloadedFilename == "plan_replayer" {
			content, err = handlePlanReplayerCaptureFile(content, path, dfHandler)
			if err != nil {
				handler.WriteError(w, err)
				return
			}
		}
		_, err = w.Write(content)
		if err != nil {
			handler.WriteError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.zip\"", dfHandler.downloadedFilename))
		logutil.BgLogger().Info("return dump file successfully", zap.String("filename", name),
			zap.String("address", localAddr), zap.Bool("forwarded", isForwarded))
		return
	}
	// handler.infoGetter will be nil only in unit test
	// or we couldn't find file for forward request, return 404
	if dfHandler.infoGetter == nil || isForwarded {
		logutil.BgLogger().Info("failed to find dump file", zap.String("filename", name),
			zap.String("address", localAddr), zap.Bool("forwarded", isForwarded))
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// If we didn't find file in origin request, try to broadcast the request to all remote tidb-servers
	topos, err := dfHandler.infoGetter.ServerInfoSyncer().GetAllTiDBTopology(req.Context())
	if err != nil {
		handler.WriteError(w, err)
		return
	}
	client := util.InternalHTTPClient()
	// transfer each remote tidb-server and try to find dump file
	for _, topo := range topos {
		if topo.IP == dfHandler.address && topo.StatusPort == dfHandler.statusPort {
			continue
		}
		remoteAddr := net.JoinHostPort(topo.IP, strconv.Itoa(int(topo.StatusPort)))
		url := fmt.Sprintf("%s://%s/%s?forward=true", dfHandler.scheme, remoteAddr, dfHandler.urlPath)
		resp, err := client.Get(url)
		if err != nil {
			logutil.BgLogger().Error("forward request failed",
				zap.String("remote-addr", remoteAddr), zap.Error(err))
			continue
		}
		if resp.StatusCode != http.StatusOK {
			logutil.BgLogger().Info("can't find file in remote server", zap.String("filename", name),
				zap.String("remote-addr", remoteAddr), zap.Int("status-code", resp.StatusCode))
			continue
		}
		content, err := io.ReadAll(resp.Body)
		if err != nil {
			handler.WriteError(w, err)
			return
		}
		err = resp.Body.Close()
		if err != nil {
			handler.WriteError(w, err)
			return
		}
		_, err = w.Write(content)
		if err != nil {
			handler.WriteError(w, err)
			return
		}
		// find dump file in one remote tidb-server, return file directly
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.zip\"", dfHandler.downloadedFilename))
		logutil.BgLogger().Info("return dump file successfully in remote server",
			zap.String("filename", name), zap.String("remote-addr", remoteAddr))
		return
	}
	// we can't find dump file in any tidb-server, return 404 directly
	logutil.BgLogger().Info("can't find dump file in any remote server", zap.String("filename", name))
	w.WriteHeader(http.StatusNotFound)
	_, err = fmt.Fprintf(w, "can't find dump file %s in any remote server", name)
	if err != nil {
		handler.WriteError(w, err)
	}
}

type downloadFileHandler struct {
	scheme             string
	filePath           string
	fileName           string
	infoGetter         *infosync.InfoSyncer
	address            string
	statusPort         uint
	urlPath            string
	downloadedFilename string

	statsHandle *handle.Handle
	is          infoschema.InfoSchema
}

func isExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func handlePlanReplayerCaptureFile(content []byte, path string, handler downloadFileHandler) ([]byte, error) {
	if !strings.HasPrefix(handler.filePath, "capture_replayer") {
		return content, nil
	}
	b := bytes.NewReader(content)
	zr, err := zip.NewReader(b, int64(len(content)))
	if err != nil {
		return nil, err
	}
	startTS, err := loadSQLMetaFile(zr)
	if err != nil {
		return nil, err
	}
	if startTS == 0 {
		return content, nil
	}
	tbls, err := loadSchemaMeta(zr, handler.is)
	if err != nil {
		return nil, err
	}
	for _, tbl := range tbls {
		jsonStats, _, err := handler.statsHandle.DumpHistoricalStatsBySnapshot(tbl.dbName, tbl.info, startTS)
		if err != nil {
			return nil, err
		}
		tbl.jsonStats = jsonStats
	}
	newPath, err := dumpJSONStatsIntoZip(tbls, content, path)
	if err != nil {
		return nil, err
	}
	//nolint: gosec
	file, err := os.Open(newPath)
	if err != nil {
		return nil, err
	}
	content, err = io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	err = file.Close()
	if err != nil {
		return nil, err
	}
	return content, nil
}

func loadSQLMetaFile(z *zip.Reader) (uint64, error) {
	for _, zipFile := range z.File {
		if zipFile.Name == domain.PlanReplayerSQLMetaFile {
			varMap := make(map[string]string)
			v, err := zipFile.Open()
			if err != nil {
				return 0, errors.AddStack(err)
			}
			//nolint: errcheck,all_revive,revive
			defer v.Close()
			_, err = toml.NewDecoder(v).Decode(&varMap)
			if err != nil {
				return 0, errors.AddStack(err)
			}
			startTS, err := strconv.ParseUint(varMap[domain.PlanReplayerSQLMetaStartTS], 10, 64)
			if err != nil {
				return 0, err
			}
			return startTS, nil
		}
	}
	return 0, nil
}

func loadSchemaMeta(z *zip.Reader, is infoschema.InfoSchema) (map[int64]*tblInfo, error) {
	r := make(map[int64]*tblInfo, 0)
	for _, zipFile := range z.File {
		if zipFile.Name == fmt.Sprintf("schema/%v", domain.PlanReplayerSchemaMetaFile) {
			v, err := zipFile.Open()
			if err != nil {
				return nil, errors.AddStack(err)
			}
			//nolint: errcheck,all_revive,revive
			defer v.Close()
			buf := new(bytes.Buffer)
			_, err = buf.ReadFrom(v)
			if err != nil {
				return nil, errors.AddStack(err)
			}
			rows := strings.Split(buf.String(), "\n")
			for _, row := range rows {
				s := strings.Split(row, ";")
				databaseName := s[0]
				tableName := s[1]
				t, err := is.TableByName(context.Background(), ast.NewCIStr(databaseName), ast.NewCIStr(tableName))
				if err != nil {
					return nil, err
				}
				r[t.Meta().ID] = &tblInfo{
					info:    t.Meta(),
					dbName:  databaseName,
					tblName: tableName,
				}
			}
			break
		}
	}
	return r, nil
}

func dumpJSONStatsIntoZip(tbls map[int64]*tblInfo, content []byte, path string) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return "", err
	}
	newPath := strings.Replace(path, "capture_replayer", "copy_capture_replayer", 1)
	zf, err := os.Create(newPath)
	if err != nil {
		return "", err
	}
	zw := zip.NewWriter(zf)
	for _, f := range zr.File {
		err = zw.Copy(f)
		if err != nil {
			logutil.BgLogger().Error("copy plan replayer zip file failed", zap.Error(err))
			return "", err
		}
	}
	for _, tbl := range tbls {
		w, err := zw.Create(fmt.Sprintf("stats/%v.%v.json", tbl.dbName, tbl.tblName))
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(tbl.jsonStats)
		if err != nil {
			return "", err
		}
		_, err = w.Write(data)
		if err != nil {
			return "", err
		}
	}
	err = zw.Close()
	if err != nil {
		logutil.BgLogger().Error("Closing file failed", zap.Error(err))
		return "", err
	}
	err = zf.Close()
	if err != nil {
		logutil.BgLogger().Error("Closing file failed", zap.Error(err))
		return "", err
	}
	return newPath, nil
}

type tblInfo struct {
	info      *model.TableInfo
	jsonStats *util2.JSONTable
	dbName    string
	tblName   string
}
