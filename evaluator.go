package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// Tempo de vida do cache em segundos
	CACHE_TTL = 30 * time.Second
)

func sanitizeForLog(s string) string {
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\t", " ")
	return s
}

func validateInternalURL(raw string) (*neturl.URL, error) {
	parsed, err := neturl.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("url inválida: %w", err)
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("scheme não permitida: %s", parsed.Scheme)
	}

	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("host vazio")
	}

	allowedHosts := map[string]struct{}{
		"localhost":         {},
		"127.0.0.1":         {},
		"flag-service":      {},
		"targeting-service": {},
	}

	if _, ok := allowedHosts[host]; ok {
		return parsed, nil
	}

	if strings.HasSuffix(host, ".svc.cluster.local") {
		return parsed, nil
	}

	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		return parsed, nil
	}

	return nil, fmt.Errorf("host não permitido: %s", host)
}

// getDecision é o wrapper principal
func (a *App) getDecision(userID, flagName string) (bool, error) {
	// 1. Obter os dados da flag (do cache ou dos serviços)
	info, err := a.getCombinedFlagInfo(flagName)
	if err != nil {
		return false, err
	}

	// 2. Executar a lógica de avaliação
	return a.runEvaluationLogic(info, userID), nil
}

// getCombinedFlagInfo busca os dados no Redis, com fallback para os microsserviços
func (a *App) getCombinedFlagInfo(flagName string) (*CombinedFlagInfo, error) {
	cacheKey := fmt.Sprintf("flag_info:%s", flagName)

	// 1. Tentar buscar do Cache (Redis)
	val, err := a.RedisClient.Get(ctx, cacheKey).Result()
	if err == nil {
		// Cache HIT
		var info CombinedFlagInfo
		if err := json.Unmarshal([]byte(val), &info); err == nil {
			safeFlag := sanitizeForLog(flagName)
			// #nosec G706 -- valor sanitizado para log
			log.Printf("Cache HIT para flag %s", safeFlag)
			return &info, nil
		}
		// Se o unmarshal falhar, trata como cache miss
		log.Printf("Erro ao desserializar cache para flag %q: %v", flagName, err)
	}

	safeFlag := sanitizeForLog(flagName)
	// #nosec G706 -- valor sanitizado para log
	log.Printf("Cache MISS para flag %s", safeFlag)
	// 2. Cache MISS - Buscar dos serviços
	info, err := a.fetchFromServices(flagName)
	if err != nil {
		return nil, err
	}

	// 3. Salvar no Cache
	jsonData, err := json.Marshal(info)
	if err == nil {
		if err := a.RedisClient.Set(ctx, cacheKey, jsonData, CACHE_TTL).Err(); err != nil {
			safeFlag := sanitizeForLog(flagName)
			// #nosec G706 -- valor sanitizado para log
			log.Printf("erro ao gravar cache para flag %s: %v", safeFlag, err)
		}
	}

	return info, nil
}

// fetchFromServices busca dados do flag-service e targeting-service concorrentemente
func (a *App) fetchFromServices(flagName string) (*CombinedFlagInfo, error) {
	var wg sync.WaitGroup
	wg.Add(2)

	var flagInfo *Flag
	var ruleInfo *TargetingRule
	var flagErr, ruleErr error

	// Goroutine 1: Buscar do flag-service
	go func() {
		defer wg.Done()
		flagInfo, flagErr = a.fetchFlag(flagName)
	}()

	// Goroutine 2: Buscar do targeting-service
	go func() {
		defer wg.Done()
		ruleInfo, ruleErr = a.fetchRule(flagName)
	}()

	wg.Wait()

	if flagErr != nil {
		return nil, flagErr
	}
	if ruleErr != nil {
		safeFlag := sanitizeForLog(flagName)
		// #nosec G706 -- valor sanitizado para log
		log.Printf("Aviso: nenhuma regra de segmentação encontrada para %s. Usando padrão.", safeFlag)
	}

	return &CombinedFlagInfo{
		Flag: flagInfo,
		Rule: ruleInfo,
	}, nil
}

// fetchFlag (função helper)
func (a *App) fetchFlag(flagName string) (*Flag, error) {
	rawURL := fmt.Sprintf("%s/flags/%s", strings.TrimRight(a.FlagServiceURL, "/"), neturl.PathEscape(flagName))

	safeURL, err := validateInternalURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("url do flag-service inválida: %w", err)
	}

	apiKey := os.Getenv("SERVICE_API_KEY")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// #nosec G704 -- URL validada por allowlist interna em validateInternalURL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, safeURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("erro ao criar request para flag-service: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := a.HttpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("erro ao chamar flag-service: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, &NotFoundError{flagName}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("flag-service retornou status %d", resp.StatusCode)
	}

	var flag Flag
	if err := json.NewDecoder(resp.Body).Decode(&flag); err != nil {
		return nil, fmt.Errorf("erro ao desserializar resposta do flag-service: %w", err)
	}

	return &flag, nil
}

func (a *App) fetchRule(flagName string) (*TargetingRule, error) {
	rawURL := fmt.Sprintf("%s/rules/%s", strings.TrimRight(a.TargetingServiceURL, "/"), neturl.PathEscape(flagName))

	safeURL, err := validateInternalURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("url do targeting-service inválida: %w", err)
	}

	apiKey := os.Getenv("SERVICE_API_KEY")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// #nosec G704 -- URL validada por allowlist interna em validateInternalURL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, safeURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("erro ao criar request para targeting-service: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := a.HttpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("erro ao chamar targeting-service: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, &NotFoundError{flagName}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("targeting-service retornou status %d", resp.StatusCode)
	}

	var rule TargetingRule
	if err := json.NewDecoder(resp.Body).Decode(&rule); err != nil {
		return nil, fmt.Errorf("erro ao desserializar resposta do targeting-service: %w", err)
	}

	return &rule, nil
}

// runEvaluationLogic é onde a decisão é tomada
func (a *App) runEvaluationLogic(info *CombinedFlagInfo, userID string) bool {
	if info.Flag == nil || !info.Flag.IsEnabled {
		return false
	}

	if info.Rule == nil || !info.Rule.IsEnabled {
		return true
	}

	// 3. Processa a regra (só temos "PERCENTAGE" por enquanto)
	rule := info.Rule.Rules
	if rule.Type == "PERCENTAGE" {
		// Converte o 'value' (que é interface{}) para float64
		percentage, ok := rule.Value.(float64)
		if !ok {
			safeFlag := sanitizeForLog(info.Flag.Name)
			// #nosec G706 -- valor sanitizado para log
			log.Printf("Erro: valor da regra de porcentagem não é um número para a flag %s", safeFlag)
			return false
		}

		// Calcula o "bucket" do usuário (0-99)
		userBucket := getDeterministicBucket(userID + info.Flag.Name)

		if float64(userBucket) < percentage {
			return true
		}
	}

	return false
}

func getDeterministicBucket(input string) int {
	sum := sha256.Sum256([]byte(input))
	val := binary.BigEndian.Uint32(sum[:4])
	return int(val % 100)
}
