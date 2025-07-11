// Copyright 2016 PingCAP, Inc.
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

package util

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/config"
	infoschema "github.com/pingcap/tidb/pkg/infoschema/context"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/metrics"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/terror"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/collate"
	"github.com/pingcap/tidb/pkg/util/logutil"
	tlsutil "github.com/pingcap/tidb/pkg/util/tls"
	"github.com/pingcap/tipb/go-tipb"
	"go.uber.org/zap"
)

const (
	// DefaultMaxRetries indicates the max retry count.
	DefaultMaxRetries = 30
	// RetryInterval indicates retry interval.
	RetryInterval uint64 = 500
)

// RunWithRetry will run the f with backoff and retry.
// retryCnt: Max retry count
// backoff: When run f failed, it will sleep backoff * triedCount time.Millisecond.
// Function f should have two return value. The first one is an bool which indicate if the err is retryable.
// The second is if the f meet any error.
func RunWithRetry(retryCnt int, backoff uint64, f func() (bool, error)) (err error) {
	for i := 1; i <= retryCnt; i++ {
		var retryAble bool
		retryAble, err = f()
		if err == nil || !retryAble {
			return errors.Trace(err)
		}
		metrics.RetryableErrorCount.WithLabelValues(err.Error()).Inc()
		sleepTime := time.Duration(backoff*uint64(i)) * time.Millisecond
		time.Sleep(sleepTime)
	}
	return errors.Trace(err)
}

// WithRecovery wraps goroutine startup call with force recovery.
// it will dump current goroutine stack into log if catch any recover result.
//
//	exec:      execute logic function.
//	recoverFn: handler will be called after recover and before dump stack, passing `nil` means noop.
func WithRecovery(exec func(), recoverFn func(r any)) {
	defer func() {
		r := recover()
		if recoverFn != nil {
			recoverFn(r)
		}
		if r != nil {
			logutil.BgLogger().Error("panic in the recoverable goroutine",
				zap.Any("r", r),
				zap.Stack("stack trace"))
		}
	}()
	exec()
}

// Recover includes operations such as recovering, clearing，and printing information.
// It will dump current goroutine stack into log if catch any recover result.
//
//	metricsLabel: The label of PanicCounter metrics.
//	funcInfo:     Some information for the panic function.
//	recoverFn:    Handler will be called after recover and before dump stack, passing `nil` means noop.
//	quit:         If this value is true, the current program exits after recovery.
func Recover(metricsLabel, funcInfo string, recoverFn func(), quit bool) {
	//nolint: revive
	r := recover()
	if r == nil {
		return
	}

	logutil.BgLogger().Error("panic in the recoverable goroutine",
		zap.String("label", metricsLabel),
		zap.String("funcInfo", funcInfo),
		zap.Any("r", r),
		zap.Stack("stack"))
	metrics.PanicCounter.WithLabelValues(metricsLabel).Inc()

	if recoverFn != nil {
		recoverFn()
	}
	if quit {
		// Wait for metrics to be pushed.
		time.Sleep(time.Second * 15)
		os.Exit(1)
	}
}

// HasCancelled checks whether context has be cancelled.
func HasCancelled(ctx context.Context) (cancel bool) {
	select {
	case <-ctx.Done():
		cancel = true
	default:
	}
	return
}

const (
	// SyntaxErrorPrefix is the common prefix for SQL syntax error in TiDB.
	SyntaxErrorPrefix = "You have an error in your SQL syntax; check the manual that corresponds to your TiDB version for the right syntax to use"
)

// SyntaxError converts parser error to TiDB's syntax error.
func SyntaxError(err error) error {
	if err == nil {
		return nil
	}
	logutil.BgLogger().Debug("syntax error", zap.Error(err))

	// If the error is already a terror with stack, pass it through.
	if errors.HasStack(err) {
		cause := errors.Cause(err)
		if _, ok := cause.(*terror.Error); ok {
			return err
		}
	}

	return parser.ErrParse.GenWithStackByArgs(SyntaxErrorPrefix, err.Error())
}

// SyntaxWarn converts parser warn to TiDB's syntax warn.
func SyntaxWarn(err error) error {
	if err == nil {
		return nil
	}
	logutil.BgLogger().Debug("syntax error", zap.Error(err))

	// If the "err" is already a terror, pass it through.
	cause := errors.Cause(err)
	if _, ok := cause.(*terror.Error); ok {
		return err
	}

	return parser.ErrParse.FastGenByArgs(SyntaxErrorPrefix, err.Error())
}

// X509NameOnline prints pkix.Name into old X509_NAME_oneline format.
// https://www.openssl.org/docs/manmaster/man3/X509_NAME_oneline.html
func X509NameOnline(n pkix.Name) string {
	s := make([]string, 0, len(n.Names))
	for _, name := range n.Names {
		oid := name.Type.String()
		// unlike MySQL, TiDB only support check pkixAttributeTypeNames fields
		if n, exist := pkixAttributeTypeNames[oid]; exist {
			s = append(s, n+"="+fmt.Sprint(name.Value))
		}
	}
	if len(s) == 0 {
		return ""
	}
	return "/" + strings.Join(s, "/")
}

const (
	// Country is type name for country.
	Country = "C"
	// Organization is type name for organization.
	Organization = "O"
	// OrganizationalUnit is type name for organizational unit.
	OrganizationalUnit = "OU"
	// Locality is type name for locality.
	Locality = "L"
	// Email is type name for email.
	Email = "emailAddress"
	// CommonName is type name for common name.
	CommonName = "CN"
	// Province is type name for province or state.
	Province = "ST"
)

// see go/src/crypto/x509/pkix/pkix.go:attributeTypeNames
var pkixAttributeTypeNames = map[string]string{
	"2.5.4.6":              Country,
	"2.5.4.10":             Organization,
	"2.5.4.11":             OrganizationalUnit,
	"2.5.4.3":              CommonName,
	"2.5.4.5":              "SERIALNUMBER",
	"2.5.4.7":              Locality,
	"2.5.4.8":              Province,
	"2.5.4.9":              "STREET",
	"2.5.4.17":             "POSTALCODE",
	"1.2.840.113549.1.9.1": Email,
}

var pkixTypeNameAttributes = make(map[string]string)

// MockPkixAttribute generates mock AttributeTypeAndValue.
// only used for test.
func MockPkixAttribute(name, value string) pkix.AttributeTypeAndValue {
	n, exists := pkixTypeNameAttributes[name]
	if !exists {
		panic(fmt.Sprintf("unsupport mock type: %s", name))
	}
	split := strings.Split(n, ".")
	vs := make([]int, 0, len(split))
	for _, v := range split {
		i, err := strconv.Atoi(v)
		if err != nil {
			panic(err)
		}
		vs = append(vs, i)
	}
	return pkix.AttributeTypeAndValue{
		Type:  vs,
		Value: value,
	}
}

// SANType is enum value for GlobalPrivValue.SANs keys.
type SANType string

const (
	// URI indicates uri info in SAN.
	URI = SANType("URI")
	// DNS indicates dns info in SAN.
	DNS = SANType("DNS")
	// IP indicates ip info in SAN.
	IP = SANType("IP")
)

var supportSAN = map[SANType]struct{}{
	URI: {},
	DNS: {},
	IP:  {},
}

// ParseAndCheckSAN parses and check SAN str.
func ParseAndCheckSAN(san string) (map[SANType][]string, error) {
	sanMap := make(map[SANType][]string)
	sans := strings.Split(san, ",")
	for _, san := range sans {
		kv := strings.SplitN(san, ":", 2)
		if len(kv) != 2 {
			return nil, errors.Errorf("invalid SAN value %s", san)
		}
		k, v := SANType(strings.ToUpper(strings.TrimSpace(kv[0]))), strings.TrimSpace(kv[1])
		if _, s := supportSAN[k]; !s {
			return nil, errors.Errorf("unsupported SAN key %s, current only support %v", k, supportSAN)
		}
		sanMap[k] = append(sanMap[k], v)
	}
	return sanMap, nil
}

// CheckSupportX509NameOneline parses and validate input str is X509_NAME_oneline format
// and precheck check-item is supported by TiDB
// https://www.openssl.org/docs/manmaster/man3/X509_NAME_oneline.html
func CheckSupportX509NameOneline(oneline string) (err error) {
	entries := strings.Split(oneline, `/`)
	for _, entry := range entries {
		if len(entry) == 0 {
			continue
		}
		kvs := strings.Split(entry, "=")
		if len(kvs) != 2 {
			err = errors.Errorf("invalid X509_NAME input: %s", oneline)
			return
		}
		k := kvs[0]
		if _, support := pkixTypeNameAttributes[k]; !support {
			err = errors.Errorf("Unsupport check '%s' in current version TiDB", k)
			return
		}
	}
	return
}

var tlsCipherString = map[uint16]string{
	tls.TLS_RSA_WITH_RC4_128_SHA:                "RC4-SHA",
	tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA:           "DES-CBC3-SHA",
	tls.TLS_RSA_WITH_AES_128_CBC_SHA:            "AES128-SHA",
	tls.TLS_RSA_WITH_AES_256_CBC_SHA:            "AES256-SHA",
	tls.TLS_RSA_WITH_AES_128_CBC_SHA256:         "AES128-SHA256",
	tls.TLS_RSA_WITH_AES_128_GCM_SHA256:         "AES128-GCM-SHA256",
	tls.TLS_RSA_WITH_AES_256_GCM_SHA384:         "AES256-GCM-SHA384",
	tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA:        "ECDHE-ECDSA-RC4-SHA",
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA:    "ECDHE-ECDSA-AES128-SHA",
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA:    "ECDHE-ECDSA-AES256-SHA",
	tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA:          "ECDHE-RSA-RC4-SHA",
	tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA:     "ECDHE-RSA-DES-CBC3-SHA",
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA:      "ECDHE-RSA-AES128-SHA",
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA:      "ECDHE-RSA-AES256-SHA",
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256: "ECDHE-ECDSA-AES128-SHA256",
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256:   "ECDHE-RSA-AES128-SHA256",
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256:   "ECDHE-RSA-AES128-GCM-SHA256",
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256: "ECDHE-ECDSA-AES128-GCM-SHA256",
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384:   "ECDHE-RSA-AES256-GCM-SHA384",
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384: "ECDHE-ECDSA-AES256-GCM-SHA384",
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305:    "ECDHE-RSA-CHACHA20-POLY1305",
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305:  "ECDHE-ECDSA-CHACHA20-POLY1305",
	// TLS 1.3 cipher suites, compatible with mysql using '_'.
	tls.TLS_AES_128_GCM_SHA256:       "TLS_AES_128_GCM_SHA256",
	tls.TLS_AES_256_GCM_SHA384:       "TLS_AES_256_GCM_SHA384",
	tls.TLS_CHACHA20_POLY1305_SHA256: "TLS_CHACHA20_POLY1305_SHA256",
}

// SupportCipher maintains cipher supported by TiDB.
var SupportCipher = make(map[string]struct{}, len(tlsCipherString))

// TLSCipher2String convert tls num to string.
// Taken from https://testssl.sh/openssl-rfc.mapping.html .
func TLSCipher2String(n uint16) string {
	s, ok := tlsCipherString[n]
	if !ok {
		return ""
	}
	return s
}

// ColumnsToProto converts a slice of model.ColumnInfo to a slice of tipb.ColumnInfo.
func ColumnsToProto(columns []*model.ColumnInfo, pkIsHandle bool, forIndex bool, isTiFlashStore bool) []*tipb.ColumnInfo {
	cols := make([]*tipb.ColumnInfo, 0, len(columns))
	for _, c := range columns {
		col := ColumnToProto(c, forIndex, isTiFlashStore)
		// TODO: Here `PkHandle`'s meaning is changed, we will change it to `IsHandle` when tikv's old select logic
		// is abandoned.
		if (pkIsHandle && mysql.HasPriKeyFlag(c.GetFlag())) || c.ID == model.ExtraHandleID {
			col.PkHandle = true
		} else {
			col.PkHandle = false
		}
		cols = append(cols, col)
	}
	return cols
}

// ColumnToProto converts model.ColumnInfo to tipb.ColumnInfo.
func ColumnToProto(c *model.ColumnInfo, forIndex bool, isTiFlashStore bool) *tipb.ColumnInfo {
	pc := &tipb.ColumnInfo{
		ColumnId:  c.ID,
		Collation: collate.RewriteNewCollationIDIfNeeded(int32(mysql.CollationNames[c.GetCollate()])),
		ColumnLen: int32(c.GetFlen()),
		Decimal:   int32(c.GetDecimal()),
		Flag:      int32(c.GetFlag()),
		Elems:     c.GetElems(),
	}
	if isTiFlashStore && c.IsVirtualGenerated() {
		pc.Flag |= int32(mysql.GeneratedColumnFlag)
	}
	if forIndex {
		// Use array type for read the multi-valued index.
		pc.Tp = int32(c.FieldType.ArrayType().GetType())
		if c.FieldType.IsArray() {
			// Use "binary" collation for read the multi-valued index. Most of the time, the `Collation` of this hidden
			// column should already been set to "binary". However, in old versions, the collation is set to the default
			// value. See https://github.com/pingcap/tidb/issues/46717
			pc.Collation = int32(mysql.CollationNames["binary"])
		}
	} else {
		pc.Tp = int32(c.GetType())
	}
	return pc
}

func init() {
	for _, value := range tlsCipherString {
		SupportCipher[value] = struct{}{}
	}
	for key, value := range pkixAttributeTypeNames {
		pkixTypeNameAttributes[value] = key
	}
}

// GetSequenceByName could be used in expression package without import cycle problem.
var GetSequenceByName func(is infoschema.MetaOnlyInfoSchema, schema, sequence ast.CIStr) (SequenceTable, error)

// SequenceTable is implemented by tableCommon,
// and it is specialised in handling sequence operation.
// Otherwise calling table will cause import cycle problem.
type SequenceTable interface {
	GetSequenceID() int64
	GetSequenceNextVal(ctx any, dbName, seqName string) (int64, error)
	SetSequenceVal(ctx any, newVal int64, dbName, seqName string) (int64, bool, error)
}

// LoadTLSCertificates loads CA/KEY/CERT for special paths.
func LoadTLSCertificates(ca, key, cert string, autoTLS bool, rsaKeySize int) (tlsConfig *tls.Config, autoReload bool, err error) {
	autoReload = false
	if len(cert) == 0 || len(key) == 0 {
		if !autoTLS {
			logutil.BgLogger().Warn("Automatic TLS Certificate creation is disabled", zap.Error(err))
			return
		}
		autoReload = true
		tempStoragePath := config.GetGlobalConfig().TempStoragePath
		cert = filepath.Join(tempStoragePath, "/cert.pem")
		key = filepath.Join(tempStoragePath, "/key.pem")
		err = createTLSCertificates(cert, key, rsaKeySize)
		if err != nil {
			logutil.BgLogger().Warn("TLS Certificate creation failed", zap.Error(err))
			return
		}
	}

	var tlsCert tls.Certificate
	tlsCert, err = tls.LoadX509KeyPair(cert, key)
	if err != nil {
		logutil.BgLogger().Warn("load x509 failed", zap.Error(err))
		err = errors.Trace(err)
		return
	}

	requireTLS := tlsutil.RequireSecureTransport.Load()

	var minTLSVersion uint16 = tls.VersionTLS12
	switch tlsver := config.GetGlobalConfig().Security.MinTLSVersion; tlsver {
	case "TLSv1.2":
		minTLSVersion = tls.VersionTLS12
	case "TLSv1.3":
		minTLSVersion = tls.VersionTLS13
	case "":
	default:
		logutil.BgLogger().Warn(
			"Invalid TLS version, using default instead",
			zap.String("tls-version", tlsver),
		)
	}
	if minTLSVersion < tls.VersionTLS12 {
		err = errors.New("Minimum TLS version pre-TLSv1.2 protocols are not allowed")
		return
	}

	// Try loading CA cert.
	clientAuthPolicy := tls.NoClientCert
	if requireTLS {
		clientAuthPolicy = tls.RequestClientCert
	}
	var certPool *x509.CertPool
	if len(ca) > 0 {
		var caCert []byte
		caCert, err = os.ReadFile(ca)
		if err != nil {
			logutil.BgLogger().Warn("read file failed", zap.Error(err))
			err = errors.Trace(err)
			return
		}
		certPool = x509.NewCertPool()
		if certPool.AppendCertsFromPEM(caCert) {
			if requireTLS {
				clientAuthPolicy = tls.RequireAndVerifyClientCert
			} else {
				clientAuthPolicy = tls.VerifyClientCertIfGiven
			}
		}
	}

	// This excludes ciphers listed in tls.InsecureCipherSuites() and can be used to filter out more
	var cipherSuites []uint16
	var cipherNames []string
	for _, sc := range tls.CipherSuites() {
		switch sc.ID {
		case tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA, tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA:
			logutil.BgLogger().Info("Disabling weak cipherSuite", zap.String("cipherSuite", sc.Name))
		default:
			cipherNames = append(cipherNames, sc.Name)
			cipherSuites = append(cipherSuites, sc.ID)
		}
	}
	logutil.BgLogger().Info("Enabled ciphersuites", zap.Strings("cipherNames", cipherNames))

	/* #nosec G402 */
	tlsConfig = &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		ClientCAs:    certPool,
		ClientAuth:   clientAuthPolicy,
		MinVersion:   minTLSVersion,
		CipherSuites: cipherSuites,
	}
	return
}

var (
	internalClientInit sync.Once
	internalHTTPClient *http.Client
	internalHTTPSchema string
)

// InternalHTTPClient is used by TiDB-Server to request other components.
func InternalHTTPClient() *http.Client {
	internalClientInit.Do(initInternalClient)
	return internalHTTPClient
}

// InternalHTTPSchema specifies use http or https to request other components.
func InternalHTTPSchema() string {
	internalClientInit.Do(initInternalClient)
	return internalHTTPSchema
}

func initInternalClient() {
	clusterSecurity := config.GetGlobalConfig().Security.ClusterSecurity()
	tlsCfg, err := clusterSecurity.ToTLSConfig()
	if err != nil {
		logutil.BgLogger().Fatal("could not load cluster ssl", zap.Error(err))
	}
	if tlsCfg == nil {
		internalHTTPSchema = "http"
		internalHTTPClient = &http.Client{
			Timeout: 5 * time.Minute,
		}
		return
	}
	internalHTTPSchema = "https"
	internalHTTPClient = &http.Client{
		Timeout:   5 * time.Minute,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
}

// ComposeURL adds HTTP schema if missing and concats address with path
func ComposeURL(address, path string) string {
	if strings.HasPrefix(address, "http://") || strings.HasPrefix(address, "https://") {
		return fmt.Sprintf("%s%s", address, path)
	}
	return fmt.Sprintf("%s://%s%s", InternalHTTPSchema(), address, path)
}

// GetLocalIP will return a local IP(non-loopback, non 0.0.0.0), if there is one
func GetLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, address := range addrs {
			ipnet, ok := address.(*net.IPNet)
			if ok && ipnet.IP.IsGlobalUnicast() {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}

// CreateCertificates creates and writes a cert based on the params.
func CreateCertificates(certpath string, keypath string, rsaKeySize int, pubKeyAlgo x509.PublicKeyAlgorithm,
	signAlgo x509.SignatureAlgorithm) error {
	certValidity := 90 * 24 * time.Hour // 90 days
	notBefore := time.Now()
	notAfter := notBefore.Add(certValidity)
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}

	template := x509.Certificate{
		Subject: pkix.Name{
			CommonName: "TiDB_Server_Auto_Generated_Server_Certificate",
		},
		SerialNumber:       big.NewInt(1),
		NotBefore:          notBefore,
		NotAfter:           notAfter,
		DNSNames:           []string{hostname},
		SignatureAlgorithm: signAlgo,
	}

	var privKey crypto.Signer
	switch pubKeyAlgo {
	case x509.RSA:
		privKey, err = rsa.GenerateKey(rand.Reader, rsaKeySize)
	case x509.ECDSA:
		privKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case x509.Ed25519:
		_, privKey, err = ed25519.GenerateKey(rand.Reader)
	default:
		return errors.Errorf("unknown public key algorithm: %s", pubKeyAlgo.String())
	}
	if err != nil {
		return err
	}
	// DER: Distinguished Encoding Rules, this is the ASN.1 encoding rule of the certificate.
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, privKey.Public(), privKey)
	if err != nil {
		return err
	}

	certOut, err := os.Create(certpath)
	if err != nil {
		return err
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return err
	}
	if err := certOut.Close(); err != nil {
		return err
	}

	keyOut, err := os.OpenFile(keypath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}

	privBytes, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		return err
	}

	if err := pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}); err != nil {
		return err
	}

	if err := keyOut.Close(); err != nil {
		return err
	}

	logutil.BgLogger().Info("TLS Certificates created", zap.String("cert", certpath), zap.String("key", keypath),
		zap.Duration("validity", certValidity), zap.Int("rsaKeySize", rsaKeySize))
	return nil
}

func createTLSCertificates(certpath string, keypath string, rsaKeySize int) error {
	// use RSA and unspecified signature algorithm
	return CreateCertificates(certpath, keypath, rsaKeySize, x509.RSA, x509.UnknownSignatureAlgorithm)
}

// GetTypeFlagsForInsert gets the type flags for insert statement.
func GetTypeFlagsForInsert(baseFlags types.Flags, sqlMode mysql.SQLMode, ignoreErr bool) types.Flags {
	strictSQLMode := sqlMode.HasStrictMode()
	// see comments in ResetContextOfStmt for WithAllowNegativeToUnsigned part.
	return baseFlags.
		WithTruncateAsWarning(!strictSQLMode || ignoreErr).
		WithIgnoreInvalidDateErr(sqlMode.HasAllowInvalidDatesMode()).
		WithIgnoreZeroInDate(!sqlMode.HasNoZeroInDateMode() ||
			!sqlMode.HasNoZeroDateMode() || !strictSQLMode || ignoreErr ||
			sqlMode.HasAllowInvalidDatesMode()).
		WithAllowNegativeToUnsigned(false)
}

// GetTypeFlagsForImportInto gets the type flags for import into statement which
// has the same flags as normal `INSERT INTO xxx`.
func GetTypeFlagsForImportInto(baseFlags types.Flags, sqlMode mysql.SQLMode) types.Flags {
	return GetTypeFlagsForInsert(baseFlags, sqlMode, false)
}
