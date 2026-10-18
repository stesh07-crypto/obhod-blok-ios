package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v3"
)

func resetLinkCache(link string) {
	norm := normalizeVKJoinHash(link)
	if norm == "" {
		norm = link
	}
	credentialsStore.mu.Lock()
	delete(credentialsStore.caches, norm)
	credentialsStore.mu.Unlock()
}

// 1. Ошибка поколения N не удаляет действительный кеш N+1.
func TestGeneration_OldGenDoesNotInvalidateNewCache(t *testing.T) {
	link := "https://vk.com/call/join/test_gen_old_1"
	resetLinkCache(link)

	fetchCount := atomic.Int32{}
	vkFetchOverride = func(ctx context.Context, l string, streamID int) (string, string, []string, error) {
		c := fetchCount.Add(1)
		return fmt.Sprintf("user_%d", c), "pass", []string{"193.203.43.46:19302"}, nil
	}
	defer func() { vkFetchOverride = nil }()

	ctx := context.Background()
	_, _, _, gen1, err := GetCredsWithGen(ctx, link, 1)
	if err != nil || gen1 != 1 {
		t.Fatalf("expected gen 1, got gen=%d, err=%v", gen1, err)
	}

	// Инвалидируем поколение 1 (штатный сброс)
	if !InvalidateLinkCacheIfGen(link, 1) {
		t.Fatalf("expected InvalidateLinkCacheIfGen for gen 1 to succeed")
	}

	// Получаем поколение 2
	_, _, _, gen2, err := GetCredsWithGen(ctx, link, 1)
	if err != nil || gen2 != 2 {
		t.Fatalf("expected gen 2, got gen=%d, err=%v", gen2, err)
	}

	// Запоздавшая ошибка поколения 1 пытается сбросить кеш поколения 2
	if InvalidateLinkCacheIfGen(link, 1) {
		t.Fatalf("InvalidateLinkCacheIfGen for old gen 1 must return false when cache is at gen 2")
	}

	// Проверяем, что кеш поколения 2 остался целым
	cache := getLinkCache(link)
	cache.mutex.RLock()
	currentGen := cache.creds.Generation
	user := cache.creds.Username
	cache.mutex.RUnlock()

	if currentGen != 2 || user != "user_2" {
		t.Fatalf("expected cache to remain at gen 2 with user_2, got gen=%d, user=%s", currentGen, user)
	}
}

// 2. Ошибка текущего ненулевого поколения инвалидирует именно его.
func TestGeneration_CurrentGenInvalidation(t *testing.T) {
	link := "https://vk.com/call/join/test_gen_curr_2"
	resetLinkCache(link)

	vkFetchOverride = func(ctx context.Context, l string, streamID int) (string, string, []string, error) {
		return "user_curr", "pass", []string{"193.203.43.46:19302"}, nil
	}
	defer func() { vkFetchOverride = nil }()

	ctx := context.Background()
	_, _, _, gen1, err := GetCredsWithGen(ctx, link, 1)
	if err != nil || gen1 != 1 {
		t.Fatalf("expected gen 1, got gen=%d, err=%v", gen1, err)
	}

	// Инвалидируем текущее поколение 1
	if !InvalidateLinkCacheIfGen(link, 1) {
		t.Fatalf("expected InvalidateLinkCacheIfGen(1) to return true")
	}

	// Проверяем, что реквизиты очищены, но монотонный счётчик сохранён
	cache := getLinkCache(link)
	cache.mutex.RLock()
	credsEmpty := (cache.creds.Link == "" && len(cache.creds.ServerAddrs) == 0)
	monoGen := cache.generation.Load()
	cache.mutex.RUnlock()

	if !credsEmpty {
		t.Fatalf("expected credentials to be cleared")
	}
	if monoGen != 1 {
		t.Fatalf("expected monotonic generation to be preserved at 1, got %d", monoGen)
	}
}

// 3. failedGen == 0 не удаляет кеш.
func TestGeneration_ZeroGenDoesNotInvalidate(t *testing.T) {
	link := "https://vk.com/call/join/test_gen_zero_3"
	resetLinkCache(link)

	vkFetchOverride = func(ctx context.Context, l string, streamID int) (string, string, []string, error) {
		return "user_zero", "pass", []string{"193.203.43.46:19302"}, nil
	}
	defer func() { vkFetchOverride = nil }()

	ctx := context.Background()
	_, _, _, gen1, err := GetCredsWithGen(ctx, link, 1)
	if err != nil || gen1 != 1 {
		t.Fatalf("expected gen 1, got gen=%d, err=%v", gen1, err)
	}

	// failedGen == 0 НЕ имеет права сбрасывать кеш
	if InvalidateLinkCacheIfGen(link, 0) {
		t.Fatalf("failedGen == 0 must return false")
	}

	// Проверяем, что кеш остался нетронутым
	cache := getLinkCache(link)
	cache.mutex.RLock()
	currentGen := cache.creds.Generation
	user := cache.creds.Username
	cache.mutex.RUnlock()

	if currentGen != 1 || user != "user_zero" {
		t.Fatalf("cache was unexpectedly modified by gen 0")
	}
}

// 4. Поколения монотонно увеличиваются после обновлений.
func TestGeneration_MonotonicallyIncrements(t *testing.T) {
	link := "https://vk.com/call/join/test_gen_mono_4"
	resetLinkCache(link)

	c := 0
	vkFetchOverride = func(ctx context.Context, l string, streamID int) (string, string, []string, error) {
		c++
		return fmt.Sprintf("user_%d", c), "pass", []string{"193.203.43.46:19302"}, nil
	}
	defer func() { vkFetchOverride = nil }()

	ctx := context.Background()
	var lastGen uint64 = 0

	for i := 1; i <= 5; i++ {
		_, _, _, gen, err := GetCredsWithGen(ctx, link, 1)
		if err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}
		if gen <= lastGen {
			t.Fatalf("iteration %d: generation did not strictly increment: got %d, last %d", i, gen, lastGen)
		}
		lastGen = gen
		if !InvalidateLinkCacheIfGen(link, gen) {
			t.Fatalf("iteration %d: failed to invalidate gen %d", i, gen)
		}
	}
}

// 5. Несколько одновременных отказов одного поколения не вызывают несколько одновременных запросов авторизации.
func TestGeneration_ConcurrentRefreshesCoalesce(t *testing.T) {
	link := "https://vk.com/call/join/test_gen_coalesce_5"
	resetLinkCache(link)

	fetchCount := atomic.Int32{}
	vkFetchOverride = func(ctx context.Context, l string, streamID int) (string, string, []string, error) {
		time.Sleep(20 * time.Millisecond) // небольшая задержка для проверки гонки
		c := fetchCount.Add(1)
		return fmt.Sprintf("user_%d", c), "pass", []string{"193.203.43.46:19302"}, nil
	}
	defer func() { vkFetchOverride = nil }()

	ctx := context.Background()
	// Инициализируем кеш
	_, _, _, gen1, _ := GetCredsWithGen(ctx, link, 1)
	if gen1 != 1 || fetchCount.Load() != 1 {
		t.Fatalf("initial fetch failed: gen=%d, fetches=%d", gen1, fetchCount.Load())
	}

	// Сбрасываем gen 1
	InvalidateLinkCacheIfGen(link, 1)

	// Запускаем 20 параллельных горутин за новыми кредами
	var wg sync.WaitGroup
	const goroutines = 20
	results := make([]uint64, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _, _, g, err := GetCredsWithGen(ctx, link, idx)
			if err != nil {
				t.Errorf("worker %d error: %v", idx, err)
			}
			results[idx] = g
		}(i)
	}
	wg.Wait()

	// Все горутины должны получить поколение 2
	for i, g := range results {
		if g != 2 {
			t.Errorf("goroutine %d got gen %d, expected 2", i, g)
		}
	}

	// Запрос к VK должен был выполниться ровно один раз (+1 начальный = 2)
	if fetchCount.Load() != 2 {
		t.Fatalf("expected exactly 2 fetches in total (1 initial + 1 refreshed), got %d", fetchCount.Load())
	}
}

// 6. После получения N+1 запоздавшие ошибки N не запускают новую авторизацию.
func TestGeneration_LateErrorsDoNotTriggerNewFetch(t *testing.T) {
	link := "https://vk.com/call/join/test_gen_late_6"
	resetLinkCache(link)

	fetchCount := atomic.Int32{}
	vkFetchOverride = func(ctx context.Context, l string, streamID int) (string, string, []string, error) {
		c := fetchCount.Add(1)
		return fmt.Sprintf("user_%d", c), "pass", []string{"193.203.43.46:19302"}, nil
	}
	defer func() { vkFetchOverride = nil }()

	ctx := context.Background()
	_, _, _, gen1, _ := GetCredsWithGen(ctx, link, 1)
	InvalidateLinkCacheIfGen(link, gen1)
	_, _, _, gen2, _ := GetCredsWithGen(ctx, link, 1)

	if gen2 != 2 || fetchCount.Load() != 2 {
		t.Fatalf("expected gen 2, fetchCount=2; got gen=%d, fetchCount=%d", gen2, fetchCount.Load())
	}

	// 10 запоздавших воркеров от поколения 1 сообщают об ошибке
	for i := 0; i < 10; i++ {
		inv := InvalidateLinkCacheIfGen(link, 1)
		if inv {
			t.Fatalf("late error for gen 1 must not invalidate gen 2 cache")
		}
		// Запрашивают креды
		_, _, _, g, err := GetCredsWithGen(ctx, link, i)
		if err != nil || g != 2 {
			t.Fatalf("late worker should get gen 2 from cache, got gen=%d, err=%v", g, err)
		}
	}

	// Количество обращений к VK не должно было увеличиться
	if fetchCount.Load() != 2 {
		t.Fatalf("fetchCount changed! Expected 2, got %d", fetchCount.Load())
	}
}

// 7. DTLS timeout, отмена контекста, stale nonce, quota, forbidden и недостаток ресурсов не вызывают сброс VK-кеша.
func TestClassification_ErrorsDoNotTriggerAuthRejection(t *testing.T) {
	// Ошибки, которые НЕ должны классифицироваться как isAuthError:
	nonAuthErrors := []error{
		errors.New("DTLS_TIMEOUT: таймаут рукопожатия DTLS (relay=193.203.43.46:19302, hs=50.001s)"),
		errors.New("DTLS_ERROR: сбой рукопожатия DTLS"),
		context.Canceled,
		context.DeadlineExceeded,
		&stun.TurnError{ErrorCodeAttr: stun.ErrorCodeAttribute{Code: stun.CodeForbidden}},
		&stun.TurnError{ErrorCodeAttr: stun.ErrorCodeAttribute{Code: stun.CodeStaleNonce}},
		&stun.TurnError{ErrorCodeAttr: stun.ErrorCodeAttribute{Code: stun.CodeAllocQuotaReached}},
		&stun.TurnError{ErrorCodeAttr: stun.ErrorCodeAttribute{Code: stun.CodeInsufficientCapacity}},
		stun.ErrAttributeNotFound,
		errors.New("TURN Allocate (193.203.43.46:19302): attribute not found"),
		errors.New("TURN_FORBIDDEN (код 403): forbidden"),
		errors.New("TURN_STALE_NONCE (код 438): stale nonce"),
		errors.New("TURN_QUOTA (код 486): allocation quota reached"),
		errors.New("TURN_CAPACITY (код 508): insufficient capacity"),
		errors.New("network is unreachable"),
		errors.New("cannot create socket"),
	}

	for _, err := range nonAuthErrors {
		if isAuthError(err) {
			t.Errorf("expected isAuthError(%v) == false, got true", err)
		}
	}

	// Ошибки, которые ДОЛЖНЫ классифицироваться как isAuthError (окончательный отказ авторизации):
	authErrors := []error{
		&stun.TurnError{ErrorCodeAttr: stun.ErrorCodeAttribute{Code: stun.CodeUnauthorized}},
		&stun.TurnError{ErrorCodeAttr: stun.ErrorCodeAttribute{Code: stun.CodeWrongCredentials}},
		errors.New("TURN_AUTH_REJECTED (код 401): unauthorized"),
		errors.New("TURN_AUTH_REJECTED (код 441): wrong credentials"),
		errors.New("TURN_AUTH_REJECTED: unauthorized"),
	}

	for _, err := range authErrors {
		if !isAuthError(err) {
			t.Errorf("expected isAuthError(%v) == true, got false", err)
		}
	}
}

// 8. Подтверждённый окончательный отказ аутентификации проходит через проверку поколения.
func TestGeneration_AuthRejectionChecksGeneration(t *testing.T) {
	link := "https://vk.com/call/join/test_gen_auth_8"
	resetLinkCache(link)

	vkFetchOverride = func(ctx context.Context, l string, streamID int) (string, string, []string, error) {
		return "user_auth", "pass", []string{"193.203.43.46:19302"}, nil
	}
	defer func() { vkFetchOverride = nil }()

	ctx := context.Background()
	_, _, _, gen1, _ := GetCredsWithGen(ctx, link, 1)

	// Неправильное поколение (0)
	if InvalidateLinkCacheIfGen(link, 0) {
		t.Fatalf("gen 0 should fail")
	}

	// Неправильное поколение (gen + 10)
	if InvalidateLinkCacheIfGen(link, gen1+10) {
		t.Fatalf("future gen should fail")
	}

	// Правильное поколение
	if !InvalidateLinkCacheIfGen(link, gen1) {
		t.Fatalf("matching gen should succeed")
	}
}

// 9. Отмена контекста прекращает ожидание без зависших горутин.
func TestContextCancellation_StopsCleanly(t *testing.T) {
	link := "https://vk.com/call/join/test_gen_ctx_9"
	resetLinkCache(link)

	vkFetchOverride = func(ctx context.Context, l string, streamID int) (string, string, []string, error) {
		select {
		case <-ctx.Done():
			return "", "", nil, ctx.Err()
		case <-time.After(1 * time.Second):
			return "user", "pass", []string{"193.203.43.46:19302"}, nil
		}
	}
	defer func() { vkFetchOverride = nil }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Отменяем немедленно

	_, _, _, _, err := GetCredsWithGen(ctx, link, 1)
	if err == nil {
		t.Fatalf("expected error from canceled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Logf("got context cancel error: %v", err)
	}
}

// 10. Повтор после неудачного обновления не повреждает состояние кеша.
func TestFailureState_DoesNotCorruptCache(t *testing.T) {
	link := "https://vk.com/call/join/test_gen_fail_10"
	resetLinkCache(link)

	failVk := true
	vkFetchOverride = func(ctx context.Context, l string, streamID int) (string, string, []string, error) {
		if failVk {
			return "", "", nil, errors.New("VK API 500 internal error")
		}
		return "user_ok", "pass", []string{"193.203.43.46:19302"}, nil
	}
	defer func() { vkFetchOverride = nil }()

	ctx := context.Background()
	_, _, _, _, err := GetCredsWithGen(ctx, link, 1)
	if err == nil {
		t.Fatalf("expected error from failing VK API")
	}

	// Кеш должен остаться пустым, а не содержать мусорные данные
	cache := getLinkCache(link)
	cache.mutex.RLock()
	linkVal := cache.creds.Link
	cache.mutex.RUnlock()

	if linkVal != "" {
		t.Fatalf("cache should not have valid link after failure, got %s", linkVal)
	}

	// После восстановления VK API запрос должен успешно вернуть валидные реквизиты
	failVk = false
	u, _, _, gen, err := GetCredsWithGen(ctx, link, 1)
	if err != nil || u != "user_ok" || gen != 1 {
		t.Fatalf("expected successful recovery: user=%s, gen=%d, err=%v", u, gen, err)
	}
}
