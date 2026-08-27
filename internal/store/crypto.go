package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// EncryptionKeyEnv 是主密钥的环境变量名，值为 32 字节的 hex 编码（64 个字符）。
const EncryptionKeyEnv = "FLUXKEYS_ENCRYPTION_KEY"

// UserKeyPrefix 是用户 API Key 的固定前缀。
const UserKeyPrefix = "fk-"

// ErrNoEncryptionKey 表示未配置主密钥。
//
// 安全约束: 火山 Key 的 secret 一旦明文落库，数据库备份、慢查询日志、
// 只读看板任一环节泄漏都等于全部 Key 泄漏。因此缺失主密钥时必须启动失败，
// 绝不静默降级为明文存储。
var ErrNoEncryptionKey = errors.New("store: 未配置 " + EncryptionKeyEnv + "（需 32 字节 hex），拒绝以明文存储密钥")

// Cipher 负责火山 Key secret 的对称加解密。
type Cipher struct {
	aead cipher.AEAD
}

// NewCipherFromEnv 从环境变量读取主密钥并构造 Cipher。
func NewCipherFromEnv() (*Cipher, error) {
	raw := strings.TrimSpace(os.Getenv(EncryptionKeyEnv))
	if raw == "" {
		return nil, ErrNoEncryptionKey
	}
	return NewCipher(raw)
}

// NewCipher 以 hex 编码的 32 字节密钥构造 Cipher。
func NewCipher(hexKey string) (*Cipher, error) {
	key, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil {
		return nil, fmt.Errorf("store: %s 不是合法 hex: %w", EncryptionKeyEnv, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("store: %s 需为 32 字节（64 hex 字符），实际 %d 字节", EncryptionKeyEnv, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("store: 构造 AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("store: 构造 GCM: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// EncryptSecret 加密明文密钥，输出 base64(nonce || ciphertext)。
//
// 每次调用使用全新随机 nonce，因此同一明文两次加密结果不同 —— 这是刻意的，
// 避免数据库中相同密文暴露"这两个 Key 用了同一个 secret"。
func (c *Cipher) EncryptSecret(plaintext string) (string, error) {
	if c == nil || c.aead == nil {
		return "", ErrNoEncryptionKey
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("store: 生成 nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptSecret 解密 EncryptSecret 的输出。
func (c *Cipher) DecryptSecret(encoded string) (string, error) {
	if c == nil || c.aead == nil {
		return "", ErrNoEncryptionKey
	}
	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("store: 密文不是合法 base64: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(sealed) < ns {
		return "", errors.New("store: 密文长度不足")
	}
	plain, err := c.aead.Open(nil, sealed[:ns], sealed[ns:], nil)
	if err != nil {
		// GCM 校验失败说明密钥不对或密文被篡改，两者都不该被容忍。
		return "", fmt.Errorf("store: 解密失败（主密钥不匹配或密文损坏）: %w", err)
	}
	return string(plain), nil
}

// HashUserKey 计算用户 API Key 的存储哈希。
//
// 明文 Key 绝不落库: 只在创建时返回一次。使用 SHA-256 而非 bcrypt 是因为
// 这是高熵随机串（128 bit）而非人类密码，不存在字典攻击面，而鉴权在请求
// 热路径上需要 O(1) 的哈希查表 —— bcrypt 每次 ~100ms 会直接压垮网关。
func HashUserKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// NewUserKey 生成一个新的用户 API Key 明文，格式 fk- + 32 位随机 hex。
func NewUserKey() (string, error) {
	b := make([]byte, 16) // 16 字节 -> 32 hex 字符
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("store: 生成 API Key: %w", err)
	}
	return UserKeyPrefix + hex.EncodeToString(b), nil
}

// KeyPrefix 返回用于展示识别的 Key 前缀（前 8 个字符）。
func KeyPrefix(plaintext string) string {
	if len(plaintext) <= 8 {
		return plaintext
	}
	return plaintext[:8]
}
