#[allow(warnings)]
mod bindings;

use bindings::Guest;

struct Component;

impl Guest for Component {
    fn run(args: Vec<String>) -> Result<Vec<u8>, String> {
        let (lhs, rhs) = parse_matrices(&args)?;
        let product = multiply(&lhs, &rhs)?;
        Ok(encode_matrix(&product).into_bytes())
    }
}

#[derive(Clone, Debug)]
struct Matrix {
    rows: usize,
    cols: usize,
    values: Vec<i64>,
}

fn parse_matrices(args: &[String]) -> Result<(Matrix, Matrix), String> {
    if args.is_empty() {
        return Ok((
            Matrix {
                rows: 2,
                cols: 2,
                values: vec![1, 2, 3, 4],
            },
            Matrix {
                rows: 2,
                cols: 2,
                values: vec![5, 6, 7, 8],
            },
        ));
    }

    if args.len() != 2 {
        return Err("expected zero args or two matrix specs like 2x2:1,2,3,4".to_string());
    }

    Ok((parse_matrix(&args[0])?, parse_matrix(&args[1])?))
}

fn parse_matrix(spec: &str) -> Result<Matrix, String> {
    let (shape, values) = spec
        .split_once(':')
        .ok_or_else(|| format!("matrix spec {spec:?} is missing ':'"))?;
    let (rows, cols) = shape
        .split_once('x')
        .ok_or_else(|| format!("matrix shape {shape:?} is missing 'x'"))?;
    let rows = rows
        .parse::<usize>()
        .map_err(|err| format!("invalid row count {rows:?}: {err}"))?;
    let cols = cols
        .parse::<usize>()
        .map_err(|err| format!("invalid column count {cols:?}: {err}"))?;
    let values = values
        .split(',')
        .map(|value| {
            value
                .parse::<i64>()
                .map_err(|err| format!("invalid matrix value {value:?}: {err}"))
        })
        .collect::<Result<Vec<_>, _>>()?;

    if rows == 0 || cols == 0 {
        return Err("matrix dimensions must be non-zero".to_string());
    }

    if values.len() != rows * cols {
        return Err(format!(
            "matrix shape {rows}x{cols} requires {} values, got {}",
            rows * cols,
            values.len()
        ));
    }

    Ok(Matrix { rows, cols, values })
}

fn multiply(lhs: &Matrix, rhs: &Matrix) -> Result<Matrix, String> {
    if lhs.cols != rhs.rows {
        return Err(format!(
            "cannot multiply {}x{} by {}x{}",
            lhs.rows, lhs.cols, rhs.rows, rhs.cols
        ));
    }

    let mut values = vec![0; lhs.rows * rhs.cols];
    for row in 0..lhs.rows {
        for col in 0..rhs.cols {
            let mut sum = 0;
            for inner in 0..lhs.cols {
                sum += lhs.values[row * lhs.cols + inner] * rhs.values[inner * rhs.cols + col];
            }
            values[row * rhs.cols + col] = sum;
        }
    }

    Ok(Matrix {
        rows: lhs.rows,
        cols: rhs.cols,
        values,
    })
}

fn encode_matrix(matrix: &Matrix) -> String {
    let values = matrix
        .values
        .iter()
        .map(i64::to_string)
        .collect::<Vec<_>>()
        .join(",");
    format!("{}x{}:{values}", matrix.rows, matrix.cols)
}

bindings::export!(Component with_types_in bindings);
