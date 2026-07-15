// cerberus-llama-forward: optional native llama.cpp helper (same JSON protocol as sidecar.py).
//
// Build against a llama.cpp checkout:
//   cmake -B build -DLLAMA_CPP_DIR=/path/to/llama.cpp && cmake --build build
//
// MATURITY: best-effort eval-callback capture; mid-layer injection is limited
// (see daemon/inference/llamacpp/README.md).

#include "llama.h"

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

static constexpr int DEMO_LAYERS = 4;

struct capture_state {
    int target_layer = -1;
    std::vector<float> data;
    int hidden_dim = 0;
};

static bool eval_cb(struct ggml_tensor * t, bool ask, void * user_data) {
    auto * st = static_cast<capture_state *>(user_data);
    if (!t || !t->name) {
        return false;
    }
    std::string name(t->name);
    if (ask) {
        if (st->target_layer < 0) {
            return false;
        }
        char pat[32];
        std::snprintf(pat, sizeof(pat), ".%d.", st->target_layer);
        return name.find(pat) != std::string::npos &&
               (name.find("attn_out") != std::string::npos ||
                name.find("ffn_out") != std::string::npos ||
                name.find("attn_norm") != std::string::npos);
    }
    const int64_t ne0 = t->ne[0];
    const int64_t ne1 = t->ne[1] > 0 ? t->ne[1] : 1;
    const int64_t n = ne0 * ne1;
    st->hidden_dim = static_cast<int>(ne0);
    st->data.resize(static_cast<size_t>(n));
    ggml_backend_tensor_get(t, st->data.data(), 0, static_cast<size_t>(n) * sizeof(float));
    return true;
}

static void map_demo_layers(int layer_lo, int layer_hi, int n_layer, int & m_lo, int & m_hi) {
    m_lo = layer_lo * n_layer / DEMO_LAYERS;
    m_hi = (layer_hi + 1) * n_layer / DEMO_LAYERS - 1;
    if (m_hi >= n_layer) {
        m_hi = n_layer - 1;
    }
    if (m_lo > m_hi) {
        m_lo = m_hi;
    }
}

static std::string json_escape(const std::string & s) {
    std::string out;
    out.reserve(s.size() + 8);
    for (char c : s) {
        switch (c) {
        case '\\': out += "\\\\"; break;
        case '"':  out += "\\\""; break;
        case '\n': out += "\\n"; break;
        case '\r': out += "\\r"; break;
        default:   out += c; break;
        }
    }
    return out;
}

static void reply_ok_activation(const std::vector<float> & hidden, int n_embd, int n_layer, int m_lo, int m_hi) {
    std::printf("{\"ok\":true,\"backend\":\"llamacpp\",\"hidden_size\":%d,\"n_layers\":%d,"
                "\"model_layer_lo\":%d,\"model_layer_hi\":%d,\"activation\":[",
                n_embd, n_layer, m_lo, m_hi);
    const int pipe_n = hidden.size() < DEMO_LAYERS ? static_cast<int>(hidden.size()) : DEMO_LAYERS;
    for (int i = 0; i < pipe_n; ++i) {
        if (i) std::printf(",");
        std::printf("%.9g", hidden[static_cast<size_t>(i)]);
    }
    for (int i = pipe_n; i < DEMO_LAYERS; ++i) {
        std::printf(",0");
    }
    std::printf("],\"hidden\":[");
    for (size_t i = 0; i < hidden.size(); ++i) {
        if (i) std::printf(",");
        std::printf("%.9g", hidden[i]);
    }
    std::printf("]}\n");
    std::fflush(stdout);
}

static void reply_err(const char * msg) {
    std::printf("{\"ok\":false,\"error\":\"%s\"}\n", json_escape(msg).c_str());
    std::fflush(stdout);
}

static int run_forward(const char * model_path, int layer_lo, int layer_hi, const std::vector<float> & activation) {
    llama_backend_init();
    llama_model_params mparams = llama_model_default_params();
    llama_model * model = llama_load_model_from_file(model_path, mparams);
    if (!model) {
        reply_err("failed to load model");
        llama_backend_free();
        return 0;
    }

    llama_context_params cparams = llama_context_default_params();
    cparams.embeddings = true;
    cparams.n_ctx = 512;
    llama_context * ctx = llama_new_context_with_model(model, cparams);
    if (!ctx) {
        reply_err("failed to create context");
        llama_free_model(model);
        llama_backend_free();
        return 0;
    }

    const int n_layer = llama_n_layer(model);
    const int n_embd  = llama_n_embd(model);
    int m_lo = 0;
    int m_hi = 0;
    map_demo_layers(layer_lo, layer_hi, n_layer, m_lo, m_hi);

    capture_state cap;
    cap.target_layer = m_hi;
    llama_set_eval_callback(ctx, eval_cb, &cap);

    llama_token bos = llama_token_bos(model);
    if (bos < 0) {
        bos = 1;
    }

    std::vector<llama_token> tokens = { bos };
    llama_batch batch = llama_batch_get_one(tokens.data(), static_cast<int32_t>(tokens.size()));

    std::vector<float> embd;
    if (!activation.empty() && m_lo == 0) {
        embd.assign(static_cast<size_t>(n_embd), 0.0f);
        const size_t n = activation.size() < static_cast<size_t>(n_embd) ? activation.size() : static_cast<size_t>(n_embd);
        for (size_t i = 0; i < n; ++i) {
            embd[i] = activation[i];
        }
        batch.embd = embd.data();
        batch.token = nullptr;
    }

    if (llama_decode(ctx, batch) != 0) {
        reply_err("llama_decode failed");
        llama_free(ctx);
        llama_free_model(model);
        llama_backend_free();
        return 0;
    }

    std::vector<float> hidden;
    if (!cap.data.empty()) {
        hidden = cap.data;
    } else {
        const float * emb = llama_get_embeddings(ctx);
        if (!emb) {
            reply_err("no embeddings/hidden capture");
            llama_free(ctx);
            llama_free_model(model);
            llama_backend_free();
            return 0;
        }
        hidden.assign(emb, emb + n_embd);
    }

    reply_ok_activation(hidden, n_embd, n_layer, m_lo, m_hi);
    llama_free(ctx);
    llama_free_model(model);
    llama_backend_free();
    return 0;
}

// Minimal JSON scanner for v0.1 requests (no external deps).
static bool parse_forward_req(const std::string & line, std::string & model, int & lo, int & hi, std::vector<float> & act) {
    const char * mp = std::strstr(line.c_str(), "\"model\"");
    if (mp) {
        const char * q1 = std::strchr(mp, '"');
        if (q1) {
            q1 = std::strchr(q1 + 1, '"');
            if (q1) {
                const char * q2 = std::strchr(q1 + 1, '"');
                if (q2) {
                    model.assign(q1 + 1, q2);
                }
            }
        }
    }
    auto parse_int = [&](const char * key, int & out) {
        const char * p = std::strstr(line.c_str(), key);
        if (!p) return;
        p = std::strchr(p, ':');
        if (!p) return;
        out = std::atoi(p + 1);
    };
    parse_int("\"layer_lo\"", lo);
    parse_int("\"layer_hi\"", hi);

    const char * ap = std::strstr(line.c_str(), "\"activation\"");
    if (!ap) {
        return true;
    }
    const char * lb = std::strchr(ap, '[');
    const char * rb = lb ? std::strchr(lb, ']') : nullptr;
    if (!lb || !rb) {
        return true;
    }
    std::string arr(lb + 1, rb);
    size_t start = 0;
    while (start < arr.size()) {
        size_t comma = arr.find(',', start);
        if (comma == std::string::npos) comma = arr.size();
        std::string tok = arr.substr(start, comma - start);
        if (!tok.empty()) {
            act.push_back(static_cast<float>(std::atof(tok.c_str())));
        }
        start = comma + 1;
    }
    return true;
}

int main() {
    std::string line;
    while (std::getline(std::cin, line)) {
        if (line.empty()) {
            continue;
        }
        if (line.find("\"op\":\"ping\"") != std::string::npos || line.find("\"op\": \"ping\"") != std::string::npos) {
            std::printf("{\"ok\":true}\n");
            std::fflush(stdout);
            continue;
        }
        if (line.find("\"op\":\"shutdown\"") != std::string::npos) {
            std::printf("{\"ok\":true}\n");
            std::fflush(stdout);
            return 0;
        }
        if (line.find("\"op\":\"info\"") != std::string::npos) {
            const char * env_model = std::getenv("CERBERUS_LLAMA_MODEL");
            if (!env_model) {
                env_model = std::getenv("LLAMA_MODEL");
            }
            std::printf("{\"ok\":true,\"llama_cpp\":true,\"model\":\"%s\",\"demo_layers\":%d}\n",
                        env_model ? json_escape(env_model).c_str() : "",
                        DEMO_LAYERS);
            std::fflush(stdout);
            continue;
        }
        if (line.find("\"op\":\"forward\"") == std::string::npos) {
            reply_err("unknown op");
            continue;
        }
        std::string model;
        int lo = 0;
        int hi = 0;
        std::vector<float> act;
        parse_forward_req(line, model, lo, hi, act);
        if (model.empty()) {
            const char * env_model = std::getenv("CERBERUS_LLAMA_MODEL");
            if (!env_model) {
                env_model = std::getenv("LLAMA_MODEL");
            }
            if (env_model) {
                model = env_model;
            }
        }
        if (model.empty()) {
            reply_err("no GGUF model path");
            continue;
        }
        run_forward(model.c_str(), lo, hi, act);
    }
    return 0;
}
