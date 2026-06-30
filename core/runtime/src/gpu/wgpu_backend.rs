//! Real **wgpu** GPU-dispatch backend (behind the default-OFF `gpu` feature).
//!
//! This is a genuine GPU path: each [`KernelSource`] is translated to a WGSL
//! compute shader, input buffers are uploaded to GPU storage buffers, the shader
//! is dispatched, and the result is read back. It runs on whatever backend wgpu
//! selects (Vulkan / Metal / DX12 / GL).
//!
//! Honesty note (CLAUDE.md "maturity honesty"): on a headless or GPU-less host
//! there may be **no adapter**, in which case [`WgpuDispatch::new`] returns
//! [`GpuError::NoAdapter`] and the caller should fall back to [`super::SoftwareGpu`].
//! We never fabricate a result when no GPU ran. Numerically the WGSL kernels match
//! the software backend, so a host with a GPU gets identical outputs.

use bytemuck;
use wgpu::util::DeviceExt;

use super::{validate, GpuDispatch, GpuError, KernelSource};

/// A wgpu device+queue ready to dispatch compute kernels.
pub struct WgpuDispatch {
    device: wgpu::Device,
    queue: wgpu::Queue,
    /// Human-readable adapter name (backend + device), for logging/labelling.
    adapter_info: String,
}

impl WgpuDispatch {
    /// Acquire a GPU device. Returns [`GpuError::NoAdapter`] if no adapter is
    /// available (the expected outcome on a headless CI box with no GPU/driver).
    pub fn new() -> Result<Self, GpuError> {
        pollster::block_on(Self::new_async())
    }

    async fn new_async() -> Result<Self, GpuError> {
        let instance = wgpu::Instance::new(&wgpu::InstanceDescriptor::default());
        let adapter = instance
            .request_adapter(&wgpu::RequestAdapterOptions {
                power_preference: wgpu::PowerPreference::HighPerformance,
                compatible_surface: None,
                force_fallback_adapter: false,
            })
            .await
            .map_err(|_| GpuError::NoAdapter)?;

        let info = adapter.get_info();
        let adapter_info = format!("{} ({:?})", info.name, info.backend);

        let (device, queue) = adapter
            .request_device(&wgpu::DeviceDescriptor {
                label: Some("cerberus-gpu-dispatch"),
                required_features: wgpu::Features::empty(),
                required_limits: wgpu::Limits::downlevel_defaults(),
                experimental_features: wgpu::ExperimentalFeatures::default(),
                memory_hints: wgpu::MemoryHints::Performance,
                trace: wgpu::Trace::Off,
            })
            .await
            .map_err(|e| GpuError::Backend(format!("request_device: {e}")))?;

        Ok(Self {
            device,
            queue,
            adapter_info,
        })
    }

    /// The selected adapter's name + backend (for logging which GPU actually ran).
    pub fn adapter_info(&self) -> &str {
        &self.adapter_info
    }

    /// Build the WGSL compute shader for a kernel. All kernels share the same
    /// two-storage-buffer + uniform-scalar layout; unused inputs are simply not
    /// bound (the software backend and this must agree on semantics).
    fn wgsl(kernel: &KernelSource) -> String {
        // bindings: 0 = in_a (read), 1 = in_b (read, may be unused), 2 = out (write),
        //           3 = params (uniform: scalar at .x)
        let body = match kernel {
            KernelSource::VectorAdd => "out[i] = a[i] + b[i];",
            KernelSource::Saxpy { .. } => "out[i] = params.scalar * a[i] + b[i];",
            KernelSource::ScalarMul { .. } => "out[i] = a[i] * params.scalar;",
        };
        format!(
            r#"
struct Params {{ scalar: f32, n: u32, _pad0: u32, _pad1: u32 }};
@group(0) @binding(0) var<storage, read> a: array<f32>;
@group(0) @binding(1) var<storage, read> b: array<f32>;
@group(0) @binding(2) var<storage, read_write> out: array<f32>;
@group(0) @binding(3) var<uniform> params: Params;

@compute @workgroup_size(64)
fn main(@builtin(global_invocation_id) gid: vec3<u32>) {{
    let i = gid.x;
    if (i >= params.n) {{ return; }}
    {body}
}}
"#
        )
    }

    fn scalar_of(kernel: &KernelSource) -> f32 {
        match *kernel {
            KernelSource::Saxpy { alpha } => alpha,
            KernelSource::ScalarMul { scalar } => scalar,
            KernelSource::VectorAdd => 0.0,
        }
    }
}

impl GpuDispatch for WgpuDispatch {
    fn submit(&self, kernel: &KernelSource, inputs: &[&[f32]]) -> Result<Vec<f32>, GpuError> {
        let n = validate(kernel, inputs)?;
        if n == 0 {
            return Ok(Vec::new());
        }

        let a = inputs[0];
        // Single-input kernels still bind a `b` buffer (unused by the shader) so the
        // bind-group layout is uniform; a 1-element placeholder is enough.
        let b: &[f32] = if inputs.len() == 2 { inputs[1] } else { &[0.0] };

        let device = &self.device;
        let buf_size = (n * std::mem::size_of::<f32>()) as u64;

        let a_buf = device.create_buffer_init(&wgpu::util::BufferInitDescriptor {
            label: Some("in_a"),
            contents: bytemuck::cast_slice(a),
            usage: wgpu::BufferUsages::STORAGE,
        });
        let b_buf = device.create_buffer_init(&wgpu::util::BufferInitDescriptor {
            label: Some("in_b"),
            contents: bytemuck::cast_slice(b),
            usage: wgpu::BufferUsages::STORAGE,
        });
        let out_buf = device.create_buffer(&wgpu::BufferDescriptor {
            label: Some("out"),
            size: buf_size,
            usage: wgpu::BufferUsages::STORAGE | wgpu::BufferUsages::COPY_SRC,
            mapped_at_creation: false,
        });
        let readback = device.create_buffer(&wgpu::BufferDescriptor {
            label: Some("readback"),
            size: buf_size,
            usage: wgpu::BufferUsages::MAP_READ | wgpu::BufferUsages::COPY_DST,
            mapped_at_creation: false,
        });

        // Params uniform: scalar + element count.
        #[repr(C)]
        #[derive(Clone, Copy, bytemuck::Pod, bytemuck::Zeroable)]
        struct Params {
            scalar: f32,
            n: u32,
            _pad0: u32,
            _pad1: u32,
        }
        let params = Params {
            scalar: Self::scalar_of(kernel),
            n: n as u32,
            _pad0: 0,
            _pad1: 0,
        };
        let params_buf = device.create_buffer_init(&wgpu::util::BufferInitDescriptor {
            label: Some("params"),
            contents: bytemuck::bytes_of(&params),
            usage: wgpu::BufferUsages::UNIFORM,
        });

        let shader = device.create_shader_module(wgpu::ShaderModuleDescriptor {
            label: Some("kernel"),
            source: wgpu::ShaderSource::Wgsl(Self::wgsl(kernel).into()),
        });

        let pipeline = device.create_compute_pipeline(&wgpu::ComputePipelineDescriptor {
            label: Some("compute"),
            layout: None,
            module: &shader,
            entry_point: Some("main"),
            compilation_options: wgpu::PipelineCompilationOptions::default(),
            cache: None,
        });

        let bind_group = device.create_bind_group(&wgpu::BindGroupDescriptor {
            label: Some("bind"),
            layout: &pipeline.get_bind_group_layout(0),
            entries: &[
                wgpu::BindGroupEntry {
                    binding: 0,
                    resource: a_buf.as_entire_binding(),
                },
                wgpu::BindGroupEntry {
                    binding: 1,
                    resource: b_buf.as_entire_binding(),
                },
                wgpu::BindGroupEntry {
                    binding: 2,
                    resource: out_buf.as_entire_binding(),
                },
                wgpu::BindGroupEntry {
                    binding: 3,
                    resource: params_buf.as_entire_binding(),
                },
            ],
        });

        let mut encoder =
            device.create_command_encoder(&wgpu::CommandEncoderDescriptor { label: Some("enc") });
        {
            let mut pass = encoder.begin_compute_pass(&wgpu::ComputePassDescriptor {
                label: Some("pass"),
                timestamp_writes: None,
            });
            pass.set_pipeline(&pipeline);
            pass.set_bind_group(0, &bind_group, &[]);
            let groups = n.div_ceil(64) as u32;
            pass.dispatch_workgroups(groups, 1, 1);
        }
        encoder.copy_buffer_to_buffer(&out_buf, 0, &readback, 0, buf_size);
        self.queue.submit(Some(encoder.finish()));

        // Map the readback buffer and block until the GPU is done.
        let slice = readback.slice(..);
        let (tx, rx) = std::sync::mpsc::channel();
        slice.map_async(wgpu::MapMode::Read, move |res| {
            let _ = tx.send(res);
        });
        self.device
            .poll(wgpu::PollType::wait_indefinitely())
            .map_err(|e| GpuError::Backend(format!("poll: {e:?}")))?;
        rx.recv()
            .map_err(|e| GpuError::Backend(format!("map channel: {e}")))?
            .map_err(|e| GpuError::Backend(format!("map_async: {e}")))?;

        let data = slice.get_mapped_range();
        let out: Vec<f32> = bytemuck::cast_slice(&data).to_vec();
        drop(data);
        readback.unmap();
        Ok(out)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gpu::SoftwareGpu;

    /// Acquire a GPU or skip. On a headless/GPU-less host `WgpuDispatch::new`
    /// returns `NoAdapter`; we treat that as "skip", NOT a failure — the real GPU
    /// path is only asserted where a GPU actually exists (CLAUDE.md honesty).
    fn gpu_or_skip() -> Option<WgpuDispatch> {
        match WgpuDispatch::new() {
            Ok(g) => {
                eprintln!("wgpu adapter: {}", g.adapter_info());
                Some(g)
            }
            Err(GpuError::NoAdapter) => {
                eprintln!("no GPU adapter present — skipping real-wgpu test (software path covers correctness)");
                None
            }
            Err(e) => panic!("unexpected wgpu init error: {e}"),
        }
    }

    #[test]
    fn wgpu_vector_add_matches_software() {
        let Some(gpu) = gpu_or_skip() else { return };
        let a: Vec<f32> = (0..1000).map(|i| i as f32).collect();
        let b: Vec<f32> = (0..1000).map(|i| (2 * i) as f32).collect();
        let got = gpu
            .submit(&KernelSource::VectorAdd, &[&a, &b])
            .expect("gpu run");
        let want = SoftwareGpu
            .submit(&KernelSource::VectorAdd, &[&a, &b])
            .unwrap();
        assert_eq!(
            got, want,
            "GPU vector-add must match the software reference"
        );
    }

    #[test]
    fn wgpu_saxpy_matches_software() {
        let Some(gpu) = gpu_or_skip() else { return };
        let x: Vec<f32> = (0..256).map(|i| i as f32 * 0.5).collect();
        let y: Vec<f32> = (0..256).map(|i| i as f32).collect();
        let k = KernelSource::Saxpy { alpha: 3.0 };
        let got = gpu.submit(&k, &[&x, &y]).expect("gpu run");
        let want = SoftwareGpu.submit(&k, &[&x, &y]).unwrap();
        assert_eq!(got, want, "GPU SAXPY must match the software reference");
    }

    #[test]
    fn wgpu_rejects_bad_inputs_like_software() {
        let Some(gpu) = gpu_or_skip() else { return };
        let a = [1.0f32, 2.0];
        let b = [1.0f32];
        // Validation happens before any GPU work, so this errors identically.
        assert!(matches!(
            gpu.submit(&KernelSource::VectorAdd, &[&a, &b]),
            Err(GpuError::BadInputs(_))
        ));
    }
}
