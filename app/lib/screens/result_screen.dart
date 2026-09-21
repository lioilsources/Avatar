import 'dart:async';
import 'dart:typed_data';

import 'package:flutter/material.dart';

import '../models/preset.dart';
import '../models/render_job.dart';
import '../services/api_client.dart';

/// Obrazovka s průběhem renderu a výsledkem.
class ResultScreen extends StatefulWidget {
  final ApiClient api;
  final RenderJob job;
  final Preset preset;
  final String? faceId;

  const ResultScreen({
    super.key,
    required this.api,
    required this.job,
    required this.preset,
    this.faceId,
  });

  @override
  State<ResultScreen> createState() => _ResultScreenState();
}

class _ResultScreenState extends State<ResultScreen> {
  late RenderJob _job;
  StreamSubscription<RenderJob>? _sub;
  Timer? _pollTimer;
  Uint8List? _image;
  String? _error;

  TrainStatus? _train;
  Timer? _trainTimer;
  bool _rerendering = false;

  @override
  void initState() {
    super.initState();
    _job = widget.job;
    _watch();
    if (widget.faceId != null) _watchTraining();
  }

  @override
  void dispose() {
    _sub?.cancel();
    _pollTimer?.cancel();
    _trainTimer?.cancel();
    super.dispose();
  }

  /// Sleduje trénink na pozadí. Interval je dlouhý schválně — je to
  /// nenápadná informace, ne něco, na co uživatel čeká (§9.1).
  void _watchTraining() {
    _trainTimer = Timer.periodic(const Duration(seconds: 5), (t) async {
      try {
        final st = await widget.api.trainingStatus(widget.faceId!);
        if (!mounted) return;
        setState(() => _train = st);
        if (!st.inProgress) t.cancel();
      } catch (_) {
        // Nedostupný stav tréninku nic nerozbíjí — výsledek už je vidět.
      }
    });
  }

  /// Zrenderuje tentýž recept znovu, teď už s natrénovanou LoRA.
  Future<void> _rerender() async {
    final recipeId = _job.recipeId;
    if (recipeId == null || _rerendering) return;

    setState(() {
      _rerendering = true;
      _image = null;
      _error = null;
    });

    try {
      final job = await widget.api.rerender(recipeId);
      if (!mounted) return;
      setState(() => _job = job);
      _sub?.cancel();
      _watch();
    } catch (e) {
      if (!mounted) return;
      setState(() => _error = e.toString());
    } finally {
      if (mounted) setState(() => _rerendering = false);
    }
  }

  void _watch() {
    _sub = widget.api.watchRender(_job.id).listen(
      _apply,
      onError: (_) {
        // SSE se nepovedlo — spadneme na polling. Uživatel nemá poznat
        // rozdíl, jen se progress bude aktualizovat po sekundách.
        _startPolling();
      },
      onDone: () {
        // Stream skončil bez terminálního stavu — dotáhneme ho dotazem.
        if (!_job.isTerminal) _refreshOnce();
      },
    );
  }

  void _startPolling() {
    _pollTimer?.cancel();
    _pollTimer = Timer.periodic(const Duration(seconds: 1), (t) async {
      if (_job.isTerminal) {
        t.cancel();
        return;
      }
      await _refreshOnce();
    });
  }

  Future<void> _refreshOnce() async {
    try {
      _apply(await widget.api.getRender(_job.id));
    } catch (_) {
      // Jeden neúspěšný dotaz nic neznamená — další tik to zkusí znovu.
    }
  }

  void _apply(RenderJob j) {
    if (!mounted) return;
    setState(() => _job = j);

    if (j.status == JobStatus.done && _image == null) {
      _loadImage();
    } else if (j.status == JobStatus.error) {
      setState(() => _error = j.error ?? 'render selhal');
    }
  }

  Future<void> _loadImage() async {
    try {
      final bytes = await widget.api.fetchResult(_job.id);
      if (!mounted) return;
      setState(() => _image = bytes);
    } catch (e) {
      if (!mounted) return;
      setState(() => _error = e.toString());
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: Text(widget.preset.displayName)),
      body: Center(
        child: Padding(
          padding: const EdgeInsets.all(20),
          child: switch (_job.status) {
            JobStatus.done when _image != null => _Result(
                image: _image!,
                durationMs: _job.durationMs,
                degradedAttrs: _job.degradedAttrs,
                train: _train,
                onRerender: _rerender,
              ),
            JobStatus.error => _Failure(message: _error ?? 'render selhal'),
            _ => _Progress(job: _job),
          },
        ),
      ),
    );
  }
}

class _Progress extends StatelessWidget {
  final RenderJob job;
  const _Progress({required this.job});

  @override
  Widget build(BuildContext context) {
    // Dokud nepřijde první procento, ukážeme neurčitý indikátor —
    // pruh zaseknutý na nule vypadá jako zamrzlá appka.
    final value = job.percent > 0 ? job.percent / 100 : null;

    return Column(
      mainAxisSize: MainAxisSize.min,
      children: [
        SizedBox(
          width: 120,
          height: 120,
          child: CircularProgressIndicator(value: value, strokeWidth: 6),
        ),
        const SizedBox(height: 24),
        Text(
          switch (job.status) {
            JobStatus.queued => 'Ve frontě…',
            JobStatus.running => job.stage.isEmpty ? 'Renderuju…' : job.stage,
            _ => 'Pracuju…',
          },
          style: Theme.of(context).textTheme.titleMedium,
        ),
        if (job.percent > 0) ...[
          const SizedBox(height: 4),
          Text('${job.percent.toStringAsFixed(0)} %'),
        ],
      ],
    );
  }
}

class _Result extends StatelessWidget {
  final Uint8List image;
  final int? durationMs;
  final List<String> degradedAttrs;
  final TrainStatus? train;
  final VoidCallback? onRerender;

  const _Result({
    required this.image,
    this.durationMs,
    this.degradedAttrs = const [],
    this.train,
    this.onRerender,
  });

  @override
  Widget build(BuildContext context) {
    return Column(
      mainAxisSize: MainAxisSize.min,
      children: [
        Expanded(
          child: InteractiveViewer(
            maxScale: 4,
            child: ClipRRect(
              borderRadius: BorderRadius.circular(16),
              child: Image.memory(image),
            ),
          ),
        ),
        const SizedBox(height: 12),
        if (durationMs != null)
          Text(
            'Hotovo za ${(durationMs! / 1000).toStringAsFixed(1)} s',
            style: Theme.of(context).textTheme.bodySmall,
          ),
        if (degradedAttrs.isNotEmpty) ...[
          const SizedBox(height: 8),
          Text(
            'Atributy ${degradedAttrs.join(", ")} zatím jedou jen přes prompt.',
            style: Theme.of(context).textTheme.bodySmall,
            textAlign: TextAlign.center,
          ),
        ],
        if (train != null) _TrainNotice(train: train!, onRerender: onRerender),
        const SizedBox(height: 12),
        OutlinedButton.icon(
          onPressed: () => Navigator.of(context).pop(),
          icon: const Icon(Icons.refresh),
          label: const Text('Zkusit jiný styl'),
        ),
      ],
    );
  }
}

/// Nenápadná informace o tréninku na pozadí a nabídka rerenderu,
/// jakmile je identita povýšená na face_v2.
class _TrainNotice extends StatelessWidget {
  final TrainStatus train;
  final VoidCallback? onRerender;

  const _TrainNotice({required this.train, this.onRerender});

  @override
  Widget build(BuildContext context) {
    final small = Theme.of(context).textTheme.bodySmall;

    if (train.inProgress) {
      return Padding(
        padding: const EdgeInsets.only(top: 8),
        child: Row(
          mainAxisAlignment: MainAxisAlignment.center,
          children: [
            SizedBox(
              width: 12,
              height: 12,
              child: CircularProgressIndicator(
                strokeWidth: 2,
                value: train.percent > 0 ? train.percent / 100 : null,
              ),
            ),
            const SizedBox(width: 8),
            Text('Dolaďuji tvůj avatar…', style: small),
          ],
        ),
      );
    }

    if (train.isUpgraded) {
      return Padding(
        padding: const EdgeInsets.only(top: 8),
        child: FilledButton.tonalIcon(
          onPressed: onRerender,
          icon: const Icon(Icons.auto_fix_high, size: 18),
          label: const Text('Tvůj avatar je připravený — přegenerovat'),
        ),
      );
    }

    // Selhaný trénink se uživateli neukazuje. Appka funguje dál,
    // jen o něco hůř — není co hlásit.
    return const SizedBox.shrink();
  }
}

class _Failure extends StatelessWidget {
  final String message;
  const _Failure({required this.message});

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    return Column(
      mainAxisSize: MainAxisSize.min,
      children: [
        Icon(Icons.error_outline, size: 56, color: scheme.error),
        const SizedBox(height: 16),
        Text('Render selhal', style: Theme.of(context).textTheme.titleMedium),
        const SizedBox(height: 8),
        Text(message, textAlign: TextAlign.center),
        const SizedBox(height: 20),
        OutlinedButton(
          onPressed: () => Navigator.of(context).pop(),
          child: const Text('Zpět'),
        ),
      ],
    );
  }
}
