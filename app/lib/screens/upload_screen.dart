import 'dart:async';
import 'dart:typed_data';

import 'package:flutter/material.dart';
import 'package:image_picker/image_picker.dart';

import '../models/preset.dart';
import '../models/render_job.dart';
import '../services/api_client.dart';
import '../services/quality_gate.dart';
import 'result_screen.dart';

/// Jedna vybraná fotka i s výsledkem quality gate.
class _Photo {
  final String name;
  final Uint8List bytes;
  final QualityResult? quality;
  const _Photo(this.name, this.bytes, this.quality);

  bool get ok => quality?.accepted ?? false;
}

class UploadScreen extends StatefulWidget {
  final ApiClient api;
  const UploadScreen({super.key, required this.api});

  @override
  State<UploadScreen> createState() => _UploadScreenState();
}

class _UploadScreenState extends State<UploadScreen> {
  static const int _maxPhotos = 5;

  final _picker = ImagePicker();
  final List<_Photo> _photos = [];

  List<Preset> _presets = const [];
  Preset? _selected;
  bool _busy = false;
  String? _error;

  @override
  void initState() {
    super.initState();
    _loadPresets();
  }

  Future<void> _loadPresets() async {
    try {
      final list = await widget.api.listPresets();
      if (!mounted) return;
      setState(() {
        _presets = list;
        // Preview preset je pro slider drag, ne pro první dojem —
        // ve výchozím výběru chceme finální kvalitu.
        _selected = list.where((p) => p.profile == 'final').firstOrNull ??
            list.firstOrNull;
      });
    } catch (e) {
      if (!mounted) return;
      setState(() => _error = 'Nepodařilo se načíst presety: $e');
    }
  }

  Future<void> _pick() async {
    final remaining = _maxPhotos - _photos.length;
    if (remaining <= 0) return;

    final picked = await _picker.pickMultiImage(limit: remaining);
    if (picked.isEmpty) return;

    setState(() => _busy = true);

    for (final x in picked.take(remaining)) {
      final bytes = await x.readAsBytes();
      // Quality gate běží před uploadem — nekvalitní fotka telefon
      // vůbec neopustí (§3.1).
      final q = QualityGate.check(bytes);
      if (!mounted) return;
      setState(() => _photos.add(_Photo(x.name, bytes, q)));
    }

    if (!mounted) return;
    setState(() => _busy = false);
  }

  Future<void> _generate() async {
    final preset = _selected;
    if (preset == null || _photos.isEmpty) return;

    setState(() {
      _busy = true;
      _error = null;
    });

    try {
      final usable = _photos.where((p) => p.ok).toList();
      final faceId = await widget.api.createFace(
        usable.map((p) => (name: p.name, bytes: p.bytes)).toList(),
      );

      // Trénink LoRA se pouští hned, ale na pozadí a bez čekání (§9.1 krok 3).
      // Když selže, uživatel to nepozná — appka jede dál na instant tracku.
      unawaited(widget.api.startTraining(faceId).catchError((Object e) {
        debugPrint('trénink se nepodařilo spustit, jedeme na instant tracku: $e');
        return const TrainStatus(
          faceId: '', faceVersion: 1, state: TrainState.failed);
      }));

      final job = await widget.api.createRender(faceId: faceId, presetId: preset.id);

      if (!mounted) return;
      await Navigator.of(context).push(
        MaterialPageRoute(
          builder: (_) => ResultScreen(
            api: widget.api, job: job, preset: preset, faceId: faceId),
        ),
      );
    } catch (e) {
      if (!mounted) return;
      setState(() => _error = e.toString());
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final usable = _photos.where((p) => p.ok).length;
    final canGenerate = usable > 0 && _selected != null && !_busy;

    return Scaffold(
      appBar: AppBar(title: const Text('Avatar Studio')),
      body: ListView(
        padding: const EdgeInsets.all(16),
        children: [
          Text('Vyber 3–5 fotek', style: Theme.of(context).textTheme.titleMedium),
          const SizedBox(height: 4),
          Text(
            'Víc fotek z různých úhlů a světel dá výrazně lepší podobu.',
            style: Theme.of(context).textTheme.bodySmall,
          ),
          const SizedBox(height: 12),

          _PhotoGrid(
            photos: _photos,
            onAdd: _photos.length < _maxPhotos && !_busy ? _pick : null,
            onRemove: (i) => setState(() => _photos.removeAt(i)),
          ),

          for (final (i, p) in _photos.indexed)
            if (p.quality?.hasIssues ?? false)
              _QualityNotice(index: i + 1, quality: p.quality!),

          const SizedBox(height: 20),
          Text('Styl', style: Theme.of(context).textTheme.titleMedium),
          const SizedBox(height: 8),

          if (_presets.isEmpty)
            const Text('Načítám presety…')
          else
            Wrap(
              spacing: 8,
              runSpacing: 8,
              children: [
                for (final p in _presets)
                  ChoiceChip(
                    label: Text(p.displayName),
                    selected: _selected?.id == p.id,
                    onSelected: (_) => setState(() => _selected = p),
                  ),
              ],
            ),

          if (_error != null) ...[
            const SizedBox(height: 16),
            _ErrorBox(message: _error!),
          ],

          const SizedBox(height: 24),
          FilledButton.icon(
            onPressed: canGenerate ? _generate : null,
            icon: _busy
                ? const SizedBox(
                    width: 16, height: 16,
                    child: CircularProgressIndicator(strokeWidth: 2))
                : const Icon(Icons.auto_awesome),
            label: Text(_busy ? 'Pracuju…' : 'Vytvořit avatar'),
            style: FilledButton.styleFrom(
              padding: const EdgeInsets.symmetric(vertical: 16),
            ),
          ),

          if (_photos.isNotEmpty && usable == 0) ...[
            const SizedBox(height: 8),
            Text(
              'Ani jedna fotka neprošla kontrolou kvality.',
              style: TextStyle(color: Theme.of(context).colorScheme.error),
            ),
          ],
        ],
      ),
    );
  }
}

class _PhotoGrid extends StatelessWidget {
  final List<_Photo> photos;
  final VoidCallback? onAdd;
  final void Function(int) onRemove;

  const _PhotoGrid({required this.photos, required this.onAdd, required this.onRemove});

  @override
  Widget build(BuildContext context) {
    return Wrap(
      spacing: 8,
      runSpacing: 8,
      children: [
        for (final (i, p) in photos.indexed)
          Stack(
            children: [
              ClipRRect(
                borderRadius: BorderRadius.circular(10),
                child: Image.memory(p.bytes, width: 96, height: 96, fit: BoxFit.cover),
              ),
              // Stav kontroly kvality přímo na miniatuře — uživatel hned
              // vidí, která fotka je problém, bez hledání v seznamu.
              Positioned(
                right: 4,
                top: 4,
                child: CircleAvatar(
                  radius: 11,
                  backgroundColor: p.ok ? Colors.green.shade600 : Colors.red.shade600,
                  child: Icon(
                    p.ok ? Icons.check : Icons.priority_high,
                    size: 14,
                    color: Colors.white,
                  ),
                ),
              ),
              Positioned(
                left: 0,
                bottom: 0,
                child: IconButton(
                  icon: const Icon(Icons.close, size: 16),
                  style: IconButton.styleFrom(
                    backgroundColor: Colors.black54,
                    foregroundColor: Colors.white,
                    minimumSize: const Size(28, 28),
                    padding: EdgeInsets.zero,
                  ),
                  onPressed: () => onRemove(i),
                ),
              ),
            ],
          ),
        if (onAdd != null)
          InkWell(
            onTap: onAdd,
            borderRadius: BorderRadius.circular(10),
            child: Container(
              width: 96,
              height: 96,
              decoration: BoxDecoration(
                borderRadius: BorderRadius.circular(10),
                border: Border.all(color: Theme.of(context).dividerColor),
              ),
              child: const Icon(Icons.add_a_photo_outlined),
            ),
          ),
      ],
    );
  }
}

class _QualityNotice extends StatelessWidget {
  final int index;
  final QualityResult quality;
  const _QualityNotice({required this.index, required this.quality});

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    final blocking = quality.errors.isNotEmpty;

    return Padding(
      padding: const EdgeInsets.only(top: 8),
      child: Container(
        padding: const EdgeInsets.all(10),
        decoration: BoxDecoration(
          color: (blocking ? scheme.errorContainer : scheme.secondaryContainer)
              .withValues(alpha: 0.5),
          borderRadius: BorderRadius.circular(8),
        ),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text('Fotka $index', style: const TextStyle(fontWeight: FontWeight.w600)),
            for (final e in quality.errors) Text('• $e'),
            for (final wmsg in quality.warnings) Text('• $wmsg'),
          ],
        ),
      ),
    );
  }
}

class _ErrorBox extends StatelessWidget {
  final String message;
  const _ErrorBox({required this.message});

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    return Container(
      padding: const EdgeInsets.all(12),
      decoration: BoxDecoration(
        color: scheme.errorContainer,
        borderRadius: BorderRadius.circular(8),
      ),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Icon(Icons.error_outline, color: scheme.onErrorContainer),
          const SizedBox(width: 8),
          Expanded(child: Text(message, style: TextStyle(color: scheme.onErrorContainer))),
        ],
      ),
    );
  }
}

extension _FirstOrNull<T> on Iterable<T> {
  T? get firstOrNull => isEmpty ? null : first;
}
