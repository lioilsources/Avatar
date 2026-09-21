import 'dart:async';
import 'dart:convert';
import 'dart:typed_data';

import 'package:http/http.dart' as http;

import '../models/preset.dart';
import '../models/render_job.dart';

class ApiException implements Exception {
  final String message;
  final int? statusCode;
  ApiException(this.message, [this.statusCode]);
  @override
  String toString() => message;
}

/// Klient avatar-api.
class ApiClient {
  final String baseUrl;
  final http.Client _http;

  ApiClient({required this.baseUrl, http.Client? client})
      : _http = client ?? http.Client();

  Uri _uri(String path) => Uri.parse('$baseUrl$path');

  Future<List<Preset>> listPresets() async {
    final resp = await _http.get(_uri('/v1/presets'));
    final body = _decode(resp);
    final list = (body['presets'] as List<dynamic>? ?? const []);
    return list
        .map((e) => Preset.fromJson(e as Map<String, dynamic>))
        .toList(growable: false);
  }

  /// Nahraje fotky a založí identitu. Vrací face_id.
  Future<String> createFace(List<({String name, Uint8List bytes})> photos) async {
    final req = http.MultipartRequest('POST', _uri('/v1/faces'));
    for (final p in photos) {
      req.files.add(http.MultipartFile.fromBytes('photos', p.bytes, filename: p.name));
    }

    final streamed = await _http.send(req);
    final resp = await http.Response.fromStream(streamed);
    final body = _decode(resp);

    final id = body['face_id'] as String?;
    if (id == null) throw ApiException('server nevrátil face_id');
    return id;
  }

  /// Zařadí render do fronty. Vrací job.
  Future<RenderJob> createRender({
    required String faceId,
    required String presetId,
    Map<String, dynamic> attrs = const {},
    int? seed,
  }) async {
    final resp = await _http.post(
      _uri('/v1/renders'),
      headers: const {'Content-Type': 'application/json'},
      body: jsonEncode({
        'face_id': faceId,
        'preset_id': presetId,
        'attrs': attrs,
        'seed': ?seed,
      }),
    );
    final body = _decode(resp);
    return RenderJob.fromJson(body);
  }

  Future<RenderJob> getRender(String jobId) async {
    final resp = await _http.get(_uri('/v1/renders/$jobId'));
    return RenderJob.fromJson(_decode(resp));
  }

  String resultUrl(String jobId) => '$baseUrl/v1/renders/$jobId/result';

  /// Sleduje průběh renderu přes SSE.
  ///
  /// Server posílá `event: progress` s JSON stavem a na konci `event: end`.
  /// Když stream spadne, dopadne to na volajícího — ten má fallback
  /// na polling přes [getRender].
  Stream<RenderJob> watchRender(String jobId) async* {
    final req = http.Request('GET', _uri('/v1/renders/$jobId/events'));
    req.headers['Accept'] = 'text/event-stream';
    req.headers['Cache-Control'] = 'no-cache';

    final resp = await _http.send(req);
    if (resp.statusCode != 200) {
      throw ApiException('SSE stream selhal', resp.statusCode);
    }

    String? event;
    await for (final line in resp.stream
        .transform(utf8.decoder)
        .transform(const LineSplitter())) {
      if (line.startsWith(':')) continue; // heartbeat

      if (line.startsWith('event:')) {
        event = line.substring(6).trim();
        continue;
      }

      if (line.startsWith('data:')) {
        final payload = line.substring(5).trim();
        if (event == 'end') return;
        if (payload.isEmpty) continue;
        try {
          yield RenderJob.fromJson(jsonDecode(payload) as Map<String, dynamic>);
        } catch (_) {
          // Poškozený rámec radši přeskočíme, než abychom shodili stream.
          continue;
        }
      }
    }
  }

  /// Spustí trénink per-user LoRA na pozadí.
  ///
  /// Vrací se okamžitě — appka mezitím jede dál na instant tracku.
  Future<TrainStatus> startTraining(String faceId) async {
    final resp = await _http.post(_uri('/v1/faces/$faceId/train'));
    final body = _decode(resp);
    return TrainStatus(
      faceId: faceId,
      faceVersion: 1,
      state: _parseState(body['state'] as String?),
      steps: (body['steps'] as num?)?.toInt() ?? 0,
    );
  }

  Future<TrainStatus> trainingStatus(String faceId) async {
    final resp = await _http.get(_uri('/v1/faces/$faceId/train'));
    return TrainStatus.fromJson(_decode(resp));
  }

  /// Zrenderuje uložený recept znovu.
  ///
  /// Hlavní použití: po dotrénování LoRA dá tentýž recept výrazně
  /// podobnější výsledek, protože identita mezitím povýšila na face_v2.
  Future<RenderJob> rerender(String recipeId, {String? presetId}) async {
    final resp = await _http.post(
      _uri('/v1/renders/rerender'),
      headers: const {'Content-Type': 'application/json'},
      body: jsonEncode({'recipe_id': recipeId, 'preset_id': ?presetId}),
    );
    return RenderJob.fromJson(_decode(resp));
  }

  /// Stáhne hotový obrázek.
  Future<Uint8List> fetchResult(String jobId) async {
    final resp = await _http.get(_uri('/v1/renders/$jobId/result'));
    if (resp.statusCode != 200) {
      throw ApiException('výsledek se nepodařilo stáhnout', resp.statusCode);
    }
    return resp.bodyBytes;
  }

  TrainState _parseState(String? s) => switch (s) {
        'queued' => TrainState.queued,
        'running' => TrainState.running,
        'done' => TrainState.done,
        'failed' => TrainState.failed,
        _ => TrainState.none,
      };

  Map<String, dynamic> _decode(http.Response resp) {
    Map<String, dynamic> body;
    try {
      body = jsonDecode(utf8.decode(resp.bodyBytes)) as Map<String, dynamic>;
    } catch (_) {
      throw ApiException(
        'server vrátil neočekávanou odpověď (HTTP ${resp.statusCode})',
        resp.statusCode,
      );
    }
    if (resp.statusCode >= 400) {
      throw ApiException(body['error'] as String? ?? 'chyba serveru', resp.statusCode);
    }
    return body;
  }

  void dispose() => _http.close();
}
