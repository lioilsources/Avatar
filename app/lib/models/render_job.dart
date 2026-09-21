enum JobStatus { queued, running, done, error, unknown }

JobStatus _parseStatus(String? s) => switch (s) {
      'queued' => JobStatus.queued,
      'running' => JobStatus.running,
      'done' => JobStatus.done,
      'error' => JobStatus.error,
      _ => JobStatus.unknown,
    };

/// Stav jednoho renderu.
class RenderJob {
  final String id;
  final String? recipeId;
  final JobStatus status;
  final double percent;
  final String stage;
  final String? error;
  final int? durationMs;

  /// Atributy, které v této fázi jedou jen přes prompt, protože jejich
  /// vrstva (geometry/mask) ještě nemá vykonavatele.
  final List<String> degradedAttrs;

  /// Varování, že součet změn odtlačil vzhled od původní podoby.
  final String? identityWarning;

  const RenderJob({
    required this.id,
    required this.status,
    required this.percent,
    required this.stage,
    this.recipeId,
    this.error,
    this.durationMs,
    this.degradedAttrs = const [],
    this.identityWarning,
  });

  bool get isTerminal => status == JobStatus.done || status == JobStatus.error;

  factory RenderJob.fromJson(Map<String, dynamic> j) => RenderJob(
        id: (j['job_id'] ?? j['id']) as String,
        recipeId: j['recipe_id'] as String?,
        status: _parseStatus(j['status'] as String?),
        percent: (j['percent'] as num?)?.toDouble() ?? 0,
        stage: j['stage'] as String? ?? '',
        error: j['error'] as String?,
        durationMs: (j['duration_ms'] as num?)?.toInt(),
        degradedAttrs: (j['degraded_attrs'] as List<dynamic>? ?? const [])
            .map((e) => e as String)
            .toList(growable: false),
        identityWarning: j['identity_warning'] as String?,
      );
}

enum TrainState { none, queued, running, done, failed, unknown }

TrainState _parseTrain(String? s) => switch (s) {
      'none' => TrainState.none,
      'queued' => TrainState.queued,
      'running' => TrainState.running,
      'done' => TrainState.done,
      'failed' => TrainState.failed,
      _ => TrainState.unknown,
    };

/// Stav tréninku per-user LoRA (§4.2).
///
/// Trénink běží na pozadí a nesmí blokovat nic. Dokud nedoběhne, appka
/// jede na instant tracku — tenhle objekt slouží jen k nenápadnému
/// „dolaďuji tvůj avatar…" v UI (§9.1 krok 3).
class TrainStatus {
  final String faceId;
  final int faceVersion;
  final TrainState state;
  final double percent;
  final int step;
  final int steps;
  final String? error;

  const TrainStatus({
    required this.faceId,
    required this.faceVersion,
    required this.state,
    this.percent = 0,
    this.step = 0,
    this.steps = 0,
    this.error,
  });

  bool get inProgress =>
      state == TrainState.queued || state == TrainState.running;

  /// true, jakmile je identita povýšená na face_v2 — od té chvíle má
  /// smysl nabídnout rerender uložených receptů ve vyšší kvalitě.
  bool get isUpgraded => faceVersion >= 2;

  factory TrainStatus.fromJson(Map<String, dynamic> j) {
    final job = j['job'] as Map<String, dynamic>?;
    return TrainStatus(
      faceId: j['face_id'] as String? ?? '',
      faceVersion: (j['face_version'] as num?)?.toInt() ?? 1,
      state: _parseTrain(j['train_state'] as String?),
      percent: (job?['percent'] as num?)?.toDouble() ?? 0,
      step: (job?['step'] as num?)?.toInt() ?? 0,
      steps: (job?['steps'] as num?)?.toInt() ?? 0,
      error: job?['error'] as String?,
    );
  }
}
