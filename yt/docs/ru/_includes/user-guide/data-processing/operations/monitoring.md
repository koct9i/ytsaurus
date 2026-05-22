# Мониторинг операций: статус, прогресс, результаты, метрики и логи

На этой странице описано, как наблюдать за операцией на всем жизненном цикле:
- во время выполнения;
- сразу после завершения;
- позже, когда данные об операции в основном читаются из архива операций.

Используйте материал вместе с:
- [Обзором типов операций](../../../../user-guide/data-processing/operations/overview.md);
- [Справочником команд API](../../../../api/commands.md);
- [Отладкой MapReduce-программ](../../../../user-guide/problems/mapreduce-debug.md).

## В веб-интерфейсе

Для повседневного мониторинга начинайте со страницы **Operations**:
- проверяйте `state` операции;
- смотрите `progress` и счетчики джобов;
- открывайте карточку операции и карточки джобов;
- анализируйте failed-джобы и их stderr.

## Во время выполнения: быстрые проверки

Базовый набор API-команд:
- [`get_operation`](../../../../api/commands.md#get_operation): текущие `state`, `progress`, `brief_progress`, `alerts`, `result`;
- [`list_jobs`](../../../../api/commands.md#list_jobs): running/completed/failed джобы, включая фильтры по наличию stderr;
- [`get_job_stderr`](../../../../api/commands.md#get_job_stderr): stderr конкретного джоба.

Типичный сценарий:
1. Периодически запрашивайте `get_operation` для контроля `state` и `progress`.
2. Если прогресс не меняется или `state=failed`, используйте `list_jobs`, чтобы найти проблемные джобы.
3. Для failed-джобов читайте stderr через `get_job_stderr`, а также анализируйте `result` и `alerts` операции.

## После завершения: результат и диагностика

Когда операция перешла в финальное состояние (`completed`, `failed`, `aborted`):
- используйте `state` как итоговый вердикт;
- проверяйте `result` для деталей ошибки (при failed/aborted);
- при необходимости запрашивайте `events` и `alert_events`;
- продолжайте использовать `list_jobs` и `get_job_stderr` для диагностики на уровне джобов.

Для сценариев массового сбора stderr см. [Отладка MapReduce-программ](../../../../user-guide/problems/mapreduce-debug.md).

## Исторический поиск и архив операций

Завершенные операции со временем очищаются из Cypress, и данные читаются из `//sys/operations_archive`.

Для эффективных исторических запросов:
- используйте [`list_operations`](../../../../api/commands.md#list_operations) с `include_archive=true`;
- задавайте узкое окно времени (`from_time`, `to_time`);
- как можно раньше добавляйте фильтры (`user`, `state`, `type`, `pool`, `pool_tree`, `with_failed_jobs`, `filter`).

Замечания:
- при `include_archive=true` для архивной выборки нужны `from_time` и `to_time`;
- `get_operation` подходит и для running-, и для finished-операций, но после очистки из Cypress ответ может приходить из архива.

## Метрики и алерты

Отслеживайте здоровье операций на двух уровнях:

1. **Сигналы на уровне операции**:
   - переходы `state`;
   - `progress` / `brief_progress`;
   - operation `alerts`;
   - failed-джобы и объем stderr.

2. **Мониторинг на уровне кластера**:
   - алерты и проверки стабильности scheduler/controller-agent;
   - количественные метрики в Prometheus/Grafana;
   - качественные проверки в Odin.

Про настройку и ключевые проверки см. [Мониторинг](../../../../admin-guide/monitoring.md).

## Практический чек-лист

Если операция выглядит проблемной:
1. `get_operation` → проверьте `state`, `progress`, `alerts`, `result`.
2. `list_jobs` → локализуйте failed/stuck джобы.
3. `get_job_stderr` → проверьте конкретные ошибки джобов.
4. Если операция уже завершена и очищена из Cypress, переходите к архивному сценарию: `list_operations(include_archive=true, from_time, to_time)`.
